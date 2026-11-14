package trae

import "strings"

// outboundSelection accompanies the exact body built for this attempt. Billing
// attribution never re-resolves the client's alias or fetches a second catalog.
type outboundSelection struct {
	ConfigName   string
	Function     string
	WireModel    string
	BillingModel string
}

func selectedOutbound(config, function, wire string, models []CatalogModel) outboundSelection {
	selected := outboundSelection{ConfigName: config, Function: function, WireModel: wire, BillingModel: wire}
	if wire == "" || strings.EqualFold(wire, "auto") || strings.EqualFold(wire, "work") {
		selected.BillingModel = ""
		return selected
	}
	// Strip only transport tier suffixes confirmed by this function's selected
	// profile. A real name such as qwen3.8-max remains intact. The source config
	// and display alias are not accepted as alternate names for another model.
	if model, ok := LookupCatalogModel(models, config); ok {
		profile := model.Profiles[function]
		if wire == profile.DevModelName {
			selected.BillingModel = strings.TrimSuffix(wire, "__dev")
		} else if wire == profile.MaxModelName {
			selected.BillingModel = strings.TrimSuffix(wire, "__max")
		}
	}
	return selected
}

func (s outboundSelection) matchesBill(model string) bool {
	return s.BillingModel != "" && strings.EqualFold(strings.TrimSpace(s.BillingModel), strings.TrimSpace(model))
}
