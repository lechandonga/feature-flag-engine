package flag

import (
	"encoding/json"
	"testing"
)

// buildConfig wraps flag definitions in a v2 envelope and returns JSON.
func buildConfig(t *testing.T, flags ...map[string]interface{}) []byte {
	t.Helper()
	env := map[string]interface{}{"version": "v2", "flags": flags}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
