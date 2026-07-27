package readservice

import (
	"context"

	"github.com/goobers/goobers/internal/instance"
)

// PortalConfig is the read-model projection of the operator-supplied dashboard
// co-branding (CBR). Optional string fields are pointers so the JSON contract
// carries an explicit null (rather than an omitted key) when unset, matching
// the generated portal types.
type PortalConfig struct {
	Brand   PortalBrandResponse   `json:"brand"`
	Theme   PortalThemeResponse   `json:"theme"`
	Support PortalSupportResponse `json:"support"`
}

// PortalBrandResponse is the resolved brand identity. Name, Tagline, and
// ScopeMark always carry an effective value (built-in defaults applied);
// LogoURL and FaviconURL are null when not configured.
type PortalBrandResponse struct {
	Name       string  `json:"name"`
	Tagline    string  `json:"tagline"`
	ScopeMark  string  `json:"scopeMark"`
	LogoURL    *string `json:"logoUrl"`
	FaviconURL *string `json:"faviconUrl"`
}

// PortalThemeResponse is the optional accent-color override set. Every field is
// null unless the operator configured it.
type PortalThemeResponse struct {
	AccentLight     *string `json:"accentLight"`
	AccentDark      *string `json:"accentDark"`
	AccentSoftLight *string `json:"accentSoftLight"`
	AccentSoftDark  *string `json:"accentSoftDark"`
	AccentInkLight  *string `json:"accentInkLight"`
	AccentInkDark   *string `json:"accentInkDark"`
}

// PortalSupportResponse is the optional support-link surface. Links is always a
// non-nil slice (empty when none are configured).
type PortalSupportResponse struct {
	DocsURL   *string             `json:"docsUrl"`
	IssuesURL *string             `json:"issuesUrl"`
	ChatURL   *string             `json:"chatUrl"`
	Links     []PortalSupportLink `json:"links"`
}

// PortalSupportLink is a single labelled support destination.
type PortalSupportLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// PortalConfig returns the effective dashboard co-branding for this instance:
// the operator's configured values with built-in defaults applied. A nil
// instance config yields the defaults (EffectivePortalConfig handles the nil
// receiver).
func (s *Local) PortalConfig(_ context.Context) (PortalConfig, error) {
	return projectPortalConfig(s.sources.Config.EffectivePortalConfig()), nil
}

func projectPortalConfig(c instance.PortalConfig) PortalConfig {
	return PortalConfig{
		Brand: PortalBrandResponse{
			Name:       c.Brand.Name,
			Tagline:    c.Brand.Tagline,
			ScopeMark:  c.Brand.ScopeMark,
			LogoURL:    optionalString(c.Brand.LogoURL),
			FaviconURL: optionalString(c.Brand.FaviconURL),
		},
		Theme: PortalThemeResponse{
			AccentLight:     optionalString(c.Theme.AccentLight),
			AccentDark:      optionalString(c.Theme.AccentDark),
			AccentSoftLight: optionalString(c.Theme.AccentSoftLight),
			AccentSoftDark:  optionalString(c.Theme.AccentSoftDark),
			AccentInkLight:  optionalString(c.Theme.AccentInkLight),
			AccentInkDark:   optionalString(c.Theme.AccentInkDark),
		},
		Support: PortalSupportResponse{
			DocsURL:   optionalString(c.Support.DocsURL),
			IssuesURL: optionalString(c.Support.IssuesURL),
			ChatURL:   optionalString(c.Support.ChatURL),
			Links:     projectSupportLinks(c.Support.Links),
		},
	}
}

func projectSupportLinks(links []instance.PortalSupportLink) []PortalSupportLink {
	out := make([]PortalSupportLink, 0, len(links))
	for _, l := range links {
		out = append(out, PortalSupportLink{Label: l.Label, URL: l.URL})
	}
	return out
}

// optionalString maps an empty configured string to a null JSON value and any
// non-empty value to a pointer, so unset co-branding fields serialize as null.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
