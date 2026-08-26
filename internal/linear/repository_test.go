package linear

import (
	"reflect"
	"testing"
)

func TestScopeWhere(t *testing.T) {
	tests := []struct {
		scope Scope
		where string
		args  []any
	}{
		{Scope{}, "", nil},
		{Scope{Zip: "77494"}, " AND p.zip = $1", []any{"77494"}},
		{Scope{City: "katy", State: "tx"}, " AND lower(p.city) = $1 AND lower(p.state) = $2", []any{"katy", "tx"}},
		{Scope{State: "tx"}, " AND lower(p.state) = $1", []any{"tx"}},
	}
	for _, tt := range tests {
		where, args := scopeWhere(tt.scope)
		if where != tt.where || !reflect.DeepEqual(args, tt.args) {
			t.Errorf("%+v: got %q %v, want %q %v", tt.scope, where, args, tt.where, tt.args)
		}
	}
}

// Compile-time check that the repository satisfies the service's store.
var _ Store = (*Repository)(nil)
