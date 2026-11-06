-- Service credentials bound to a namespace (contracts/service-auth.md, "Delegated calls"): the
-- calling product's namespace, set when the credential is issued with --namespace. Only a
-- credential with one may call auth "delegated" endpoints, and the receiver's kit sends it to
-- org-api as the delegated check's caller. Existing credentials keep NULL and work as before on
-- auth "service" endpoints.
ALTER TABLE service_credentials ADD COLUMN namespace text CHECK (namespace ~ '^[a-z]{2,32}$');
