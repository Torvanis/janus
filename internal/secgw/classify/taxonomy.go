package classify

// Category is one entry of the MLCommons AI Safety hazard taxonomy as
// emitted by Llama Guard 3 and 4: the model answers `unsafe` followed by
// one or more of these codes on the next line. Taxonomy below is the ONLY
// place the codes are listed; the admin API serves it so the policy
// editor, the violations page and the engine can never disagree about
// what S7 means.
//
// Tier is an editorial grouping for the picker, not a model property:
// Critical is the set an organisation almost never has a legitimate reason
// to let through; Contextual is the set whose acceptability genuinely
// depends on the team (a creative-writing group and a customer-support
// group differ on S12). The default selection for a new check is Critical.
type Category struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Tier string `json:"tier"`
}

// Category tiers.
const (
	TierCritical   = "critical"
	TierStandard   = "standard"
	TierContextual = "contextual"
)

// Taxonomy lists every category Llama Guard can return, in code order.
var Taxonomy = []Category{
	{"S1", "Violent Crimes", TierCritical},
	{"S2", "Non-Violent Crimes", TierStandard},
	{"S3", "Sex-Related Crimes", TierCritical},
	{"S4", "Child Sexual Exploitation", TierCritical},
	{"S5", "Defamation", TierStandard},
	{"S6", "Specialized Advice", TierStandard},
	{"S7", "Privacy", TierStandard},
	{"S8", "Intellectual Property", TierStandard},
	{"S9", "Indiscriminate Weapons", TierCritical},
	{"S10", "Hate", TierStandard},
	{"S11", "Suicide & Self-Harm", TierCritical},
	{"S12", "Sexual Content", TierContextual},
	{"S13", "Elections", TierContextual},
	{"S14", "Code Interpreter Abuse", TierContextual},
}

var taxonomyByCode = func() map[string]Category {
	m := make(map[string]Category, len(Taxonomy))
	for _, c := range Taxonomy {
		m[c.Code] = c
	}
	return m
}()

// KnownCategory reports whether code is in the taxonomy.
func KnownCategory(code string) bool {
	_, ok := taxonomyByCode[code]
	return ok
}

// DefaultCategories is the initial selection for a new content_safety
// check: every Critical-tier code.
func DefaultCategories() []string {
	var out []string
	for _, c := range Taxonomy {
		if c.Tier == TierCritical {
			out = append(out, c.Code)
		}
	}
	return out
}
