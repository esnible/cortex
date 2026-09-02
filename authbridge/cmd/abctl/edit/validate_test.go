package edit

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

func validateFixtureCatalog() []apiclient.PluginCatalogEntry {
	return []apiclient.PluginCatalogEntry{
		{Name: "jwt-validation", Description: "Inbound JWT"},
		{Name: "a2a-parser", Description: "Parser"},
		{Name: "mcp-parser", Description: "MCP parser"},
		{Name: "ibac", Description: "IBAC", Requires: []string{"mcp-parser"}},
	}
}

func TestValidatePipeline_Empty(t *testing.T) {
	errs := ValidatePipeline([]byte("pipeline:\n  inbound:\n    plugins: []\n"), validateFixtureCatalog())
	if len(errs) != 0 {
		t.Fatalf("empty chains: got %v", errs)
	}
}

func TestValidatePipeline_HappyPath(t *testing.T) {
	yaml := `pipeline:
  inbound:
    plugins:
      - name: jwt-validation
      - name: a2a-parser
  outbound:
    plugins:
      - name: mcp-parser
      - name: ibac
`
	errs := ValidatePipeline([]byte(yaml), validateFixtureCatalog())
	if len(errs) != 0 {
		t.Fatalf("happy path produced errors: %+v", errs)
	}
}

func TestValidatePipeline_MissingRequires(t *testing.T) {
	yaml := `pipeline:
  outbound:
    plugins:
      - name: ibac
`
	errs := ValidatePipeline([]byte(yaml), validateFixtureCatalog())
	if len(errs) == 0 {
		t.Fatal("expected error for missing mcp-parser")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "Requires \"mcp-parser\"") {
			found = true
		}
	}
	if !found {
		t.Fatalf("error should mention mcp-parser; got %+v", errs)
	}
}

func TestValidatePipeline_MisorderedRequires(t *testing.T) {
	yaml := `pipeline:
  outbound:
    plugins:
      - name: ibac
      - name: mcp-parser
`
	errs := ValidatePipeline([]byte(yaml), validateFixtureCatalog())
	if len(errs) == 0 {
		t.Fatal("expected misorder error")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "must be <") {
			found = true
		}
	}
	if !found {
		t.Fatalf("error should call out misorder; got %+v", errs)
	}
}

func TestValidatePipeline_UnknownPlugin(t *testing.T) {
	yaml := `pipeline:
  inbound:
    plugins:
      - name: definitely-not-a-real-plugin
`
	errs := ValidatePipeline([]byte(yaml), validateFixtureCatalog())
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "Unknown plugin") {
		t.Fatalf("expected Unknown plugin error; got %+v", errs)
	}
}

func TestValidatePipeline_NilCatalogSkips(t *testing.T) {
	yaml := `pipeline:
  outbound:
    plugins:
      - name: bogus
`
	errs := ValidatePipeline([]byte(yaml), nil)
	if errs != nil {
		t.Fatalf("nil catalog should disable validation, got %+v", errs)
	}
}

// --- direction advisories -------------------------------------------------

// directionFixtureCatalog declares Directions, unlike validateFixtureCatalog
// (whose entries are all unconstrained — which is itself the back-compat
// case: a catalog from an older agent yields no advisories at all).
func directionFixtureCatalog() []apiclient.PluginCatalogEntry {
	return []apiclient.PluginCatalogEntry{
		{Name: "jwt-validation", Directions: []string{"inbound"}},
		{Name: "token-exchange", Directions: []string{"outbound"}},
		{Name: "opa", Directions: []string{"inbound", "outbound"}},
		{Name: "mcp-parser", Directions: []string{"outbound"}},
		{Name: "ibac", Directions: []string{"outbound"}, Requires: []string{"mcp-parser"}},
		{Name: "legacy", Description: "declares nothing"},
	}
}

func TestValidatePipeline_DirectionMismatchIsAdvisory(t *testing.T) {
	// jwt-validation is inbound-only, placed outbound.
	subtree := []byte("pipeline:\n  outbound:\n    plugins:\n      - name: jwt-validation\n")
	errs := ValidatePipeline(subtree, directionFixtureCatalog())
	if len(errs) != 1 {
		t.Fatalf("want exactly 1 issue, got %d: %+v", len(errs), errs)
	}
	ve := errs[0]
	if ve.Severity != SeverityWarning {
		t.Errorf("direction mismatch should be SeverityWarning, got %v", ve.Severity)
	}
	if ve.PluginName != "jwt-validation" || ve.Direction != "outbound" || ve.Position != 1 {
		t.Errorf("unexpected issue shape: %+v", ve)
	}
	if !strings.Contains(ve.Message, "inbound") {
		t.Errorf("message should name the declared direction, got %q", ve.Message)
	}
}

// Correct placement, a both-chain plugin, and an entry declaring nothing
// must all produce no advisory.
func TestValidatePipeline_DirectionNoFalsePositives(t *testing.T) {
	cases := []struct {
		name    string
		subtree string
	}{
		{"correct inbound", "pipeline:\n  inbound:\n    plugins:\n      - name: jwt-validation\n"},
		{"correct outbound", "pipeline:\n  outbound:\n    plugins:\n      - name: token-exchange\n"},
		{"both-chain inbound", "pipeline:\n  inbound:\n    plugins:\n      - name: opa\n"},
		{"both-chain outbound", "pipeline:\n  outbound:\n    plugins:\n      - name: opa\n"},
		{"unconstrained entry", "pipeline:\n  inbound:\n    plugins:\n      - name: legacy\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := ValidatePipeline([]byte(c.subtree), directionFixtureCatalog())
			if len(errs) != 0 {
				t.Errorf("expected no issues, got %+v", errs)
			}
		})
	}
}

// A hard dependency error and a direction advisory in the same edit must
// keep distinct severities: the overlay renders them under different
// banners, and only the error justifies the "reload will reject" claim.
func TestValidatePipeline_SeveritiesCoexist(t *testing.T) {
	// ibac Requires mcp-parser (absent -> error); jwt-validation is
	// misplaced outbound (-> warning).
	subtree := []byte("pipeline:\n  outbound:\n    plugins:\n" +
		"      - name: ibac\n      - name: jwt-validation\n")
	errs := ValidatePipeline(subtree, directionFixtureCatalog())

	var nErr, nWarn int
	for _, ve := range errs {
		switch ve.Severity {
		case SeverityWarning:
			nWarn++
			if ve.PluginName != "jwt-validation" {
				t.Errorf("warning should be for jwt-validation, got %q", ve.PluginName)
			}
		case SeverityError:
			nErr++
			if ve.PluginName != "ibac" {
				t.Errorf("error should be for ibac, got %q", ve.PluginName)
			}
		}
	}
	if nErr != 1 || nWarn != 1 {
		t.Fatalf("want 1 error + 1 warning, got %d + %d: %+v", nErr, nWarn, errs)
	}
}

// Every pre-existing check keeps SeverityError (the zero value), so the
// "framework reload will reject" banner stays accurate for them.
func TestValidatePipeline_ExistingChecksAreErrors(t *testing.T) {
	cases := map[string]string{
		"unmet requires": "pipeline:\n  outbound:\n    plugins:\n      - name: ibac\n",
		"unknown name":   "pipeline:\n  inbound:\n    plugins:\n      - name: nope\n",
		"misordered":     "pipeline:\n  outbound:\n    plugins:\n      - name: ibac\n      - name: mcp-parser\n",
	}
	for name, subtree := range cases {
		t.Run(name, func(t *testing.T) {
			errs := ValidatePipeline([]byte(subtree), directionFixtureCatalog())
			if len(errs) == 0 {
				t.Fatal("expected at least one issue")
			}
			for _, ve := range errs {
				if ve.Severity != SeverityError {
					t.Errorf("%s should be SeverityError, got %v (%s)", name, ve.Severity, ve.Message)
				}
			}
		})
	}
}
