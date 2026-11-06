package certificatesapi

import (
	"encoding/json"
	"testing"
)

const op = `{"id":"o1","organizationId":"org","projectId":"p","type":"IssueCertificate","action":"certificates.certificateAuthorities.issue","status":"queued","target":{"id":"t1","resourceName":"r","type":"privateCertificate"},"cancellable":false,"createdBy":{},"createdAt":"x","updatedAt":"x","startedAt":null,"finishedAt":null,"error":null}`

// A 202 replay answers the operation alone: both answers decode to it.
func TestAcceptedAndIssuedReadReplays(t *testing.T) {
	var a Accepted
	if err := json.Unmarshal([]byte(op), &a); err != nil || a.Operation == nil || a.Operation.Target.ID != "t1" || a.Resource != nil {
		t.Errorf("bare operation as Accepted: %+v %v", a, err)
	}
	if err := json.Unmarshal([]byte(`{"resource":{"id":"ca1","state":"creating"},"operation":`+op+`}`), &a); err != nil || a.Resource == nil || a.Resource.ID != "ca1" || a.Operation == nil {
		t.Errorf("envelope as Accepted: %+v %v", a, err)
	}
	var i Issued
	if err := json.Unmarshal([]byte(op), &i); err != nil || i.Operation == nil || i.Certificate() != nil {
		t.Errorf("bare operation as Issued: %+v %v", i, err)
	}
	if err := json.Unmarshal([]byte(`{"operation":`+op+`}`), &i); err != nil || i.Operation == nil || i.Certificate() != nil {
		t.Errorf("{operation} as Issued: %+v %v", i, err)
	}
	if err := json.Unmarshal([]byte(`{"id":"pct1","serial":"ab","status":"issued","certificatePem":"PEM"}`), &i); err != nil || i.Operation != nil || i.Certificate() == nil || i.Certificate().ID != "pct1" {
		t.Errorf("certificate as Issued: %+v %v", i, err)
	}
	// Neither: no certificate to claim.
	if err := json.Unmarshal([]byte(`{}`), &i); err != nil || i.Certificate() != nil || i.Operation != nil {
		t.Errorf("{} as Issued: %+v %v", i, err)
	}
}
