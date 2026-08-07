package resource

import "testing"

func TestValidateCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Resource)
		wantErr bool
	}{
		{name: "valid"},
		{name: "wrong api version", mutate: func(r *Resource) { r.APIVersion = "v1" }, wantErr: true},
		{name: "invalid kind", mutate: func(r *Resource) { r.Kind = "server" }, wantErr: true},
		{name: "invalid name", mutate: func(r *Resource) { r.Metadata.Name = "Lab_Node" }, wantErr: true},
		{name: "missing spec", mutate: func(r *Resource) { r.Spec = nil }, wantErr: true},
		{name: "caller uid", mutate: func(r *Resource) { r.Metadata.UID = "chosen" }, wantErr: true},
		{name: "caller status", mutate: func(r *Resource) { r.Status = map[string]any{"ready": true} }, wantErr: true},
		{name: "unsupported finalizer", mutate: func(r *Resource) { r.Metadata.Finalizers = []string{"homelab.io/cleanup"} }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			candidate := Resource{
				APIVersion: APIVersion,
				Kind:       "Server",
				Metadata:   Metadata{Name: "lab-node"},
				Spec:       map[string]any{},
			}
			if tt.mutate != nil {
				tt.mutate(&candidate)
			}
			if got := ValidateCreate(candidate); (got != nil) != tt.wantErr {
				t.Fatalf("ValidateCreate() error = %v, wantErr %v", got, tt.wantErr)
			}
		})
	}
}
