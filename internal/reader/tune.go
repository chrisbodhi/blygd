package reader

import (
	"math"
	"sort"
)

// Ratings tune a filter on the fly, instead of a labelled set up front
// (DESIGN.md "Filters", ratings). Each rating is the reader's yes or no on
// an item, beside what the classifier said of it when they rated. The
// threshold that agrees with the most ratings is the filter's best guess
// at what the reader means.

// Ratings summarizes a filter's usable ratings: those about its current
// question (words and prompt) from the current model.
type Ratings struct {
	Rated int `json:"rated"`
	Yes   int `json:"yes"` // rated about the topic
	No    int `json:"no"`
	// Disagree counts ratings the filter's current match and threshold
	// would decide the other way.
	Disagree int `json:"disagree"`
	// Suggested is a threshold that disagrees with fewer ratings, when
	// there is one. A filter that tunes itself has already taken it.
	Suggested *float64 `json:"suggested_threshold"`
}

type rating struct {
	yes                bool
	pMentions, pMainly float64
}

func (r rating) p(match string) float64 {
	if match == "mentions" {
		return r.pMentions
	}
	return r.pMainly
}

// Tuning needs at least this many ratings each way: one of each fits any
// threshold between them, and says little.
const tuneMinEach = 2

// tuneBounds keeps a tuned threshold off the ends, where one surprising
// answer could make a filter match everything or nothing.
const tuneLo, tuneHi = 0.05, 0.95

// disagreements counts ratings that match and threshold decide the other
// way: a yes below the threshold, a no at or above it.
func disagreements(rs []rating, match string, t float64) int {
	n := 0
	for _, r := range rs {
		if r.yes != (r.p(match) >= t) {
			n++
		}
	}
	return n
}

// tune summarizes rs against the filter's match and current threshold,
// and suggests a better threshold when the ratings support one: at least
// tuneMinEach each way, and strictly fewer disagreements. Among the best,
// it takes the one nearest the current threshold, so a filter moves no
// further than its ratings make it.
func tune(rs []rating, match string, current float64) Ratings {
	out := Ratings{Rated: len(rs), Disagree: disagreements(rs, match, current)}
	for _, r := range rs {
		if r.yes {
			out.Yes++
		} else {
			out.No++
		}
	}
	if out.Yes < tuneMinEach || out.No < tuneMinEach || out.Disagree == 0 {
		return out
	}
	// Only a threshold between two answers can decide differently from
	// its neighbours, so the candidates are the midpoints.
	ps := make([]float64, 0, len(rs))
	for _, r := range rs {
		ps = append(ps, r.p(match))
	}
	sort.Float64s(ps)
	best, bestErr := current, out.Disagree
	for i := 1; i < len(ps); i++ {
		if ps[i] == ps[i-1] {
			continue
		}
		t := clampTune(round2((ps[i-1] + ps[i]) / 2))
		if t <= ps[i-1] || t > ps[i] {
			t = clampTune((ps[i-1] + ps[i]) / 2) // rounding crossed an answer
		}
		e := disagreements(rs, match, t)
		if e < bestErr || (e == bestErr && math.Abs(t-current) < math.Abs(best-current)) {
			best, bestErr = t, e
		}
	}
	if bestErr < out.Disagree {
		out.Suggested = &best
	}
	return out
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func clampTune(t float64) float64 { return min(max(t, tuneLo), tuneHi) }
