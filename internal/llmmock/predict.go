package llmmock

import (
	"errors"
	"strings"

	"github.com/kashishdesai01/modelgate/internal/model"
)

var ErrExplicitFailure = errors.New("explicit mock failure")

func Predict(text string) (model.Prediction, error) {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "[mock:error]") {
		return model.Prediction{}, ErrExplicitFailure
	}
	if strings.Contains(lower, "[mock:invalid]") {
		return model.Prediction{Category: "invented", Urgency: "soon"}, nil
	}
	category := "other"
	categories := []struct {
		label    string
		keywords []string
	}{
		{"plumbing", []string{"pipe", "sink", "toilet", "faucet", "water"}},
		{"electrical", []string{"breaker", "outlet", "wiring", "spark"}},
		{"hvac", []string{"furnace", "air conditioner", "thermostat", "heat pump"}},
		{"cleaning", []string{"clean", "carpet", "windows", "bathroom"}},
		{"landscaping", []string{"lawn", "hedge", "garden", "tree"}},
		{"roofing", []string{"roof", "shingle", "gutter", "skylight"}},
		{"painting", []string{"paint", "ceiling", "wall"}},
		{"appliance_repair", []string{"refrigerator", "washer", "oven", "dishwasher"}},
		{"handyman", []string{"shelf", "door", "drywall", "cabinet"}},
	}
	for _, candidate := range categories {
		for _, keyword := range candidate.keywords {
			if strings.Contains(lower, keyword) {
				category = candidate.label
				break
			}
		}
		if category != "other" {
			break
		}
	}
	urgency := "routine"
	if containsAny(lower, "this week", "next few days", "stopped working", "growing problem") {
		urgency = "soon"
	}
	if containsAny(lower, "flood", "spark", "smoke", "unsafe", "right now", "floding", "sparkng", "unsfe") {
		urgency = "emergency"
	}
	categoryConfidence, urgencyConfidence := 0.95, 0.95
	return model.Prediction{
		Category: category, Urgency: urgency,
		CategoryConfidence: &categoryConfidence, UrgencyConfidence: &urgencyConfidence,
	}, nil
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}
