package model

import (
	"fmt"
	"math"
)

var validCategories = map[string]struct{}{
	"plumbing": {}, "electrical": {}, "hvac": {}, "cleaning": {}, "landscaping": {},
	"roofing": {}, "painting": {}, "appliance_repair": {}, "handyman": {}, "other": {},
}

var validUrgencies = map[string]struct{}{"routine": {}, "soon": {}, "emergency": {}}

type Prediction struct {
	Category           string   `json:"category"`
	Urgency            string   `json:"urgency"`
	CategoryConfidence *float64 `json:"category_confidence,omitempty"`
	UrgencyConfidence  *float64 `json:"urgency_confidence,omitempty"`
}

func (p Prediction) Validate() error {
	if _, ok := validCategories[p.Category]; !ok {
		return fmt.Errorf("unknown category %q", p.Category)
	}
	if _, ok := validUrgencies[p.Urgency]; !ok {
		return fmt.Errorf("unknown urgency %q", p.Urgency)
	}
	for name, value := range map[string]*float64{
		"category_confidence": p.CategoryConfidence,
		"urgency_confidence":  p.UrgencyConfidence,
	} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1) {
			return fmt.Errorf("%s must be finite and between 0 and 1", name)
		}
	}
	return nil
}

type Policy struct {
	Version           string
	CategoryThreshold float64
	UrgencyThreshold  float64
	PreserveEmergency bool
}

type RoutingDecision struct {
	UseFallback bool
	Reason      string
}

func (p Policy) Decide(primary Prediction) RoutingDecision {
	if p.PreserveEmergency && primary.Urgency == "emergency" {
		return RoutingDecision{Reason: "primary_emergency_preserved"}
	}
	if primary.CategoryConfidence != nil && *primary.CategoryConfidence < p.CategoryThreshold {
		return RoutingDecision{UseFallback: true, Reason: "low_category_confidence"}
	}
	if primary.UrgencyConfidence != nil && *primary.UrgencyConfidence < p.UrgencyThreshold {
		return RoutingDecision{UseFallback: true, Reason: "low_urgency_confidence"}
	}
	return RoutingDecision{Reason: "primary_confident"}
}
