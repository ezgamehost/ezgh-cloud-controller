package l7

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"
)

// crdSchema is one served version's schema of a Gateway API CRD, as the apiserver checks it.
type crdSchema struct {
	openapi    validation.SchemaValidator
	structural *structuralschema.Structural
	cel        *cel.Validator
}

var crdSchemas = sync.OnceValues(func() (map[string]crdSchema, error) {
	// The bootstrap manifest carries the pinned release's standard CRDs, the ones clusters run.
	src, err := os.ReadFile("../../../cmd/ezgh-cloud/bootstrap/manifest.yaml")
	if err != nil {
		return nil, err
	}
	out := map[string]crdSchema{}
	for _, doc := range strings.Split(string(src), "\n---\n") {
		if !strings.Contains(doc, "\nkind: CustomResourceDefinition") || !strings.Contains(doc, "group: gateway.networking.k8s.io") {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal([]byte(doc), &crd); err != nil {
			return nil, err
		}
		for _, v := range crd.Spec.Versions {
			var in apiextensions.CustomResourceValidation
			if err := apiextensionsv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(v.Schema, &in, nil); err != nil {
				return nil, err
			}
			openapi, _, err := validation.NewSchemaValidator(in.OpenAPIV3Schema)
			if err != nil {
				return nil, err
			}
			s, err := structuralschema.NewStructural(in.OpenAPIV3Schema)
			if err != nil {
				return nil, err
			}
			out[crd.Spec.Group+"/"+v.Name+"/"+crd.Spec.Names.Kind] = crdSchema{openapi, s, cel.NewValidator(s, true, celconfig.PerCallLimit)}
		}
	}
	return out, nil
})

// validateCRD checks obj against its Gateway API CRD's schema and CEL rules (a 422 from the
// apiserver); other kinds pass.
func validateCRD(obj client.Object, scheme *runtime.Scheme) error {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return err
	}
	schemas, err := crdSchemas()
	if err != nil {
		return err
	}
	s, ok := schemas[gvk.Group+"/"+gvk.Version+"/"+gvk.Kind]
	if !ok {
		return nil
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u["apiVersion"], u["kind"] = gvk.GroupVersion().String(), gvk.Kind
	defaulting.Default(u, s.structural) // as the apiserver does before validating (fixtures leave defaults out)
	errs := append(validation.ValidateCustomResource(nil, u, s.openapi), listtype.ValidateListSetsAndMaps(nil, s.structural, u)...)
	if len(errs) == 0 {
		celErrs, _ := s.cel.Validate(context.Background(), nil, s.structural, u, nil, celconfig.RuntimeCELCostBudget)
		errs = celErrs
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s %q is invalid: %v", gvk.Kind, obj.GetName(), errs.ToAggregate())
	}
	return nil
}

// crdChecked refuses status writes the apiserver would refuse.
var crdChecked = interceptor.Funcs{
	SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if err := validateCRD(obj, c.Scheme()); err != nil {
			return err
		}
		return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
	},
	SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if err := validateCRD(obj, c.Scheme()); err != nil {
			return err
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	},
}

func TestCRDSchemasLoad(t *testing.T) {
	s, err := crdSchemas()
	if err != nil || len(s) == 0 {
		t.Fatal(len(s), err)
	}
	for _, k := range []string{"Gateway", "HTTPRoute", "GatewayClass"} {
		if _, ok := s["gateway.networking.k8s.io/v1/"+k]; !ok {
			t.Fatal("no CRD schema for", k)
		}
	}
}

// Every listener shape's status is one the CRD takes, and a second pass keeps transition times.
func TestGatewayStatusValidAndStable(t *testing.T) {
	gw := gateway(
		allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")),
		listener("tcp", gatewayv1.TCPProtocolType, 9000, ""),
		listener("clash", gatewayv1.HTTPSProtocolType, 80, ""),
		listener("nocert", gatewayv1.HTTPSProtocolType, 443, ""),
	)
	rt := route("shop", "web", 1, nil, rule(nil, backendRef("", "web", 80)))
	r, k, _, _ := newGatewayReconciler(t, gw, &rt, svc("shop", "web", port("http", 80)))
	_, _ = reconcileGateway(t, r, k) // creates the load balancer; the next pass finds it active
	first, err := reconcileGateway(t, r, k)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Status.Listeners) != 4 {
		t.Fatal(first.Status.Listeners)
	}
	second, err := reconcileGateway(t, r, k)
	if err != nil {
		t.Fatal(err)
	}
	if second.ResourceVersion != first.ResourceVersion {
		t.Fatalf("status churned:\n%+v\n%+v", first.Status, second.Status)
	}
	for _, ls := range second.Status.Listeners {
		for _, c := range ls.Conditions {
			if c.LastTransitionTime.IsZero() {
				t.Fatal(ls.Name, c)
			}
		}
	}
}
