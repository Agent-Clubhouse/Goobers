package fluidui

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"
)

// SustainedAfter is how long a user must keep a shape applied, uninterrupted,
// before it counts as sustained adoption.
const SustainedAfter = 7 * 24 * time.Hour

// ReportAdvisory states what a report is for; every rendering carries it.
const ReportAdvisory = "Advisory product proposals only. Candidates never change global defaults or any user's customization."

// Report ranks customization shapes as candidates for the standard Portal
// (#8419). It holds only counts, shapes, and need labels: no user tokens,
// identities, prompt text, panel IDs, saved-view names, or routes.
type Report struct {
	Advisory           string      `json:"advisory"`
	GeneratedAt        time.Time   `json:"generatedAt"`
	SustainedAfterDays int         `json:"sustainedAfterDays"`
	Users              int         `json:"users"`
	Candidates         []Candidate `json:"candidates"`
	UnmetNeeds         []UnmetNeed `json:"unmetNeeds"`
}

// Candidate is one customization shape and its distinct-user evidence.
type Candidate struct {
	Rank  int      `json:"rank"`
	Shape []string `json:"shape"`
	// Users applied the shape at least once; repeated revisions count once.
	Users int `json:"users"`
	// ActiveUsers have the shape applied now.
	ActiveUsers int `json:"activeUsers"`
	// SustainedUsers have kept it applied for at least SustainedAfter.
	SustainedUsers int `json:"sustainedUsers"`
	// RevertedUsers undid or reset away from the shape at least once.
	RevertedUsers int     `json:"revertedUsers"`
	ReversionRate float64 `json:"reversionRate"`
}

// UnmetNeed is a request category the schema cannot yet express.
type UnmetNeed struct {
	Label string `json:"label"`
	Users int    `json:"users"`
}

// Report ranks the shapes in the store's learning log.
func (s *Store) Report() (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := readEvents(s.Dir)
	if err != nil {
		return Report{}, err
	}
	return buildReport(events, s.now()), nil
}

type shapeTally struct {
	shape    []string
	users    map[string]bool
	reverted map[string]bool
	active   int
	sustain  int
}

type userStint struct {
	key   string
	since time.Time
}

type reportBuilder struct {
	shapes  map[string]*shapeTally
	current map[string]userStint
	needs   map[string]map[string]bool
	users   map[string]bool
}

func (b *reportBuilder) tally(shape []string) *shapeTally {
	key := strings.Join(shape, ",")
	t := b.shapes[key]
	if t == nil {
		t = &shapeTally{shape: shape, users: map[string]bool{}, reverted: map[string]bool{}}
		b.shapes[key] = t
	}
	return t
}

// observe replays one event. A user's adoption of a shape is tracked as a
// stint that starts when the user's state first becomes that shape, so
// revisions within the same shape neither add users nor restart the clock.
func (b *reportBuilder) observe(e event) {
	b.users[e.User] = true
	if e.Kind == eventUnsupported {
		if e.Need != "" {
			if b.needs[e.Need] == nil {
				b.needs[e.Need] = map[string]bool{}
			}
			b.needs[e.Need][e.User] = true
		}
		return
	}
	if len(e.From) > 0 {
		from := b.tally(e.From)
		from.users[e.User] = true
		if e.Kind == eventReverted && !slices.Equal(e.From, e.Shape) {
			from.reverted[e.User] = true
		}
	}
	if len(e.Shape) == 0 {
		delete(b.current, e.User)
		return
	}
	b.tally(e.Shape).users[e.User] = true
	key := strings.Join(e.Shape, ",")
	if b.current[e.User].key != key {
		b.current[e.User] = userStint{key: key, since: e.At}
	}
}

func buildReport(events []event, now time.Time) Report {
	slices.SortStableFunc(events, func(a, b event) int { return a.At.Compare(b.At) })
	b := reportBuilder{shapes: map[string]*shapeTally{}, current: map[string]userStint{}, needs: map[string]map[string]bool{}, users: map[string]bool{}}
	for _, e := range events {
		b.observe(e)
	}
	for _, stint := range b.current {
		t := b.shapes[stint.key]
		t.active++
		if now.Sub(stint.since) >= SustainedAfter {
			t.sustain++
		}
	}
	report := Report{
		Advisory:           ReportAdvisory,
		GeneratedAt:        now,
		SustainedAfterDays: int(SustainedAfter / (24 * time.Hour)),
		Users:              len(b.users),
		Candidates:         []Candidate{},
		UnmetNeeds:         []UnmetNeed{},
	}
	for _, t := range b.shapes {
		rate := float64(len(t.reverted)) / float64(len(t.users))
		report.Candidates = append(report.Candidates, Candidate{
			Shape: t.shape, Users: len(t.users), ActiveUsers: t.active, SustainedUsers: t.sustain,
			RevertedUsers: len(t.reverted), ReversionRate: math.Round(rate*1000) / 1000,
		})
	}
	slices.SortFunc(report.Candidates, compareCandidates)
	for i := range report.Candidates {
		report.Candidates[i].Rank = i + 1
	}
	for label, users := range b.needs {
		report.UnmetNeeds = append(report.UnmetNeeds, UnmetNeed{Label: label, Users: len(users)})
	}
	slices.SortFunc(report.UnmetNeeds, func(a, b UnmetNeed) int {
		return cmp.Or(cmp.Compare(b.Users, a.Users), cmp.Compare(a.Label, b.Label))
	})
	return report
}

// compareCandidates ranks sustained adoption first, then a lower reversion
// rate, then more distinct users. A shape users abandon therefore ranks below
// one they keep, however often it was tried.
func compareCandidates(a, b Candidate) int {
	return cmp.Or(
		cmp.Compare(b.SustainedUsers, a.SustainedUsers),
		cmp.Compare(a.ReversionRate, b.ReversionRate),
		cmp.Compare(b.ActiveUsers, a.ActiveUsers),
		cmp.Compare(b.Users, a.Users),
		cmp.Compare(strings.Join(a.Shape, ","), strings.Join(b.Shape, ",")),
	)
}
