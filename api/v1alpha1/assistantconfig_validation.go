package v1alpha1

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"time"
)

// Limits that the schema and the validation share.
const (
	assistantMaxProviders  = 16
	assistantMaxModels     = 32
	assistantMaxModelLen   = 128
	assistantMaxCIDRs      = 16
	assistantMaxBaseURLLen = 2048
	assistantMaxProviderID = 63
)

var (
	assistantNameRE   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	assistantBaseRE   = regexp.MustCompile(`^https?://`)
	assistantExpiryRE = regexp.MustCompile(`^[1-9][0-9]*(m|h)$`)
)

// ValidateAssistantConfig checks an AssistantConfig against the rules of the
// schema and the rules the schema cannot express: CIDR syntax, the approval
// expiry duration, and the defaults. Each error names the field. A zero
// budget or an empty approval expiry means the default and passes.
func ValidateAssistantConfig(cr *AssistantConfig) error {
	if cr == nil {
		return errors.New("assistantconfig: object is nil")
	}
	if cr.Name != AssistantConfigName {
		return fmt.Errorf("assistantconfig: name %q must be %q", cr.Name, AssistantConfigName)
	}
	return validateAssistantSpec(&cr.Spec)
}

func validateAssistantSpec(s *AssistantConfigSpec) error {
	if len(s.Providers) > assistantMaxProviders {
		return fmt.Errorf("assistantconfig: providers holds %d entries, the limit is %d", len(s.Providers), assistantMaxProviders)
	}
	names := make(map[string]bool, len(s.Providers))
	for i := range s.Providers {
		p := &s.Providers[i]
		if err := validateAssistantProvider(i, p); err != nil {
			return err
		}
		if names[p.Name] {
			return fmt.Errorf("assistantconfig: providers[%d].name %q is not unique", i, p.Name)
		}
		names[p.Name] = true
	}
	if err := validateAssistantDefaults(s); err != nil {
		return err
	}
	switch s.ExternalDataSharing {
	case "", AssistantSharingNone, AssistantSharingMetadata, AssistantSharingLogs:
	default:
		return fmt.Errorf("assistantconfig: externalDataSharing %q is not valid", s.ExternalDataSharing)
	}
	for field, v := range map[string]int64{
		"budgets.session":   s.Budgets.Session,
		"budgets.userDay":   s.Budgets.UserDay,
		"budgets.tenantDay": s.Budgets.TenantDay,
		"budgets.steps":     int64(s.Budgets.Steps),
	} {
		if v < 0 {
			return fmt.Errorf("assistantconfig: %s must not be negative", field)
		}
	}
	if s.ApprovalExpiry != "" {
		if !assistantExpiryRE.MatchString(s.ApprovalExpiry) {
			return fmt.Errorf("assistantconfig: approvalExpiry %q must match a number with m or h", s.ApprovalExpiry)
		}
		if _, err := time.ParseDuration(s.ApprovalExpiry); err != nil {
			return fmt.Errorf("assistantconfig: approvalExpiry %q: %w", s.ApprovalExpiry, err)
		}
	}
	return nil
}

func validateAssistantProvider(i int, p *AssistantProvider) error {
	if len(p.Name) > assistantMaxProviderID || !assistantNameRE.MatchString(p.Name) {
		return fmt.Errorf("assistantconfig: providers[%d].name %q is not a valid DNS label", i, p.Name)
	}
	switch p.Kind {
	case AssistantProviderKindAnthropic:
	case AssistantProviderKindOpenAICompatible:
		if p.BaseURL == "" {
			return fmt.Errorf("assistantconfig: providers[%d].baseURL is required for openai-compatible", i)
		}
	default:
		return fmt.Errorf("assistantconfig: providers[%d].kind %q is not valid", i, p.Kind)
	}
	if p.BaseURL != "" {
		if len(p.BaseURL) > assistantMaxBaseURLLen || !assistantBaseRE.MatchString(p.BaseURL) {
			return fmt.Errorf("assistantconfig: providers[%d].baseURL must start with http:// or https://", i)
		}
	}
	switch p.Trust {
	case AssistantTrustInternal, AssistantTrustExternal:
	default:
		return fmt.Errorf("assistantconfig: providers[%d].trust %q is not valid", i, p.Trust)
	}
	switch p.Auth {
	case "", AssistantAuthAPIKey, AssistantAuthNone:
	default:
		return fmt.Errorf("assistantconfig: providers[%d].auth %q is not valid", i, p.Auth)
	}
	if n := len(p.Models); n < 1 || n > assistantMaxModels {
		return fmt.Errorf("assistantconfig: providers[%d].models holds %d entries, the limit is 1 to %d", i, n, assistantMaxModels)
	}
	for j, m := range p.Models {
		if m == "" || len(m) > assistantMaxModelLen {
			return fmt.Errorf("assistantconfig: providers[%d].models[%d] must hold 1 to %d characters", i, j, assistantMaxModelLen)
		}
	}
	if len(p.CIDRs) > assistantMaxCIDRs {
		return fmt.Errorf("assistantconfig: providers[%d].cidrs holds %d entries, the limit is %d", i, len(p.CIDRs), assistantMaxCIDRs)
	}
	for j, c := range p.CIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return fmt.Errorf("assistantconfig: providers[%d].cidrs[%d] %q is not a CIDR", i, j, c)
		}
	}
	if p.Port < 0 || p.Port > 65535 {
		return fmt.Errorf("assistantconfig: providers[%d].port %d is out of range", i, p.Port)
	}
	return nil
}

// validateAssistantDefaults checks defaultProvider and defaultModel against
// the listed providers.
func validateAssistantDefaults(s *AssistantConfigSpec) error {
	if len(s.Providers) == 0 {
		if s.DefaultProvider != "" {
			return fmt.Errorf("assistantconfig: defaultProvider %q names no listed provider", s.DefaultProvider)
		}
		if s.DefaultModel != "" {
			return fmt.Errorf("assistantconfig: defaultModel is set with no listed provider")
		}
		return nil
	}
	if s.DefaultProvider == "" {
		return errors.New("assistantconfig: defaultProvider is required when providers are listed")
	}
	for i := range s.Providers {
		if s.Providers[i].Name != s.DefaultProvider {
			continue
		}
		if s.DefaultModel == "" {
			return nil
		}
		for _, m := range s.Providers[i].Models {
			if m == s.DefaultModel {
				return nil
			}
		}
		return fmt.Errorf("assistantconfig: defaultModel %q is not a model of provider %q", s.DefaultModel, s.DefaultProvider)
	}
	return fmt.Errorf("assistantconfig: defaultProvider %q names no listed provider", s.DefaultProvider)
}
