package profilesync

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestLookup(t *testing.T) {
	var rec map[string]any
	_ = json.Unmarshal([]byte(`{
		"userName": "bob",
		"emails": [{"value": "b@old.lab"}, {"value": "bob@sterling.lab", "primary": true}],
		"groups": [{"value": "g1", "display": "advisors"}, {"value": "g2", "display": "compliance"}],
		"user_metadata": {"department": "wealth"},
		"urn:ietf:params:scim:schemas:extension:gluu:2.0:User": {"emailVerified": true}
	}`), &rec)
	for path, want := range map[string][]string{
		"userName":                      {"bob"},
		"user_metadata.department":      {"wealth"},
		"emails[primary eq true].value": {"bob@sterling.lab"},
		"emails.value":                  {"b@old.lab", "bob@sterling.lab"},
		"groups.display":                {"advisors", "compliance"}, // a SCIM user's groups, by name
		"urn:ietf:params:scim:schemas:extension:gluu:2.0:User:emailVerified": {"true"},
		"groups.missing": nil,
		"nothing.here":   nil,
	} {
		if got := Values(Lookup(rec, path)); !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", path, got, want)
		}
	}
}
