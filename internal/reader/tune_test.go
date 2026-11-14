package reader

import "testing"

func yesAt(p float64) rating { return rating{yes: true, pMentions: p, pMainly: p} }
func noAt(p float64) rating  { return rating{yes: false, pMentions: p, pMainly: p} }

func TestTune(t *testing.T) {
	cases := []struct {
		name     string
		rs       []rating
		current  float64
		want     float64 // 0: no suggestion
		disagree int
	}{
		// 0.2 would agree with both, but one rating each way says little.
		{"too few ratings", []rating{yesAt(0.3), noAt(0.1)}, 0.5, 0, 1},
		{"too few noes", []rating{yesAt(0.3), yesAt(0.35), noAt(0.1)}, 0.5, 0, 2},
		{"already right", []rating{yesAt(0.8), yesAt(0.7), noAt(0.2), noAt(0.1)}, 0.5, 0, 0},
		// The reader says yes to posts the model put at 0.3 and 0.4: the
		// threshold comes down between the highest no and the lowest yes.
		{"lower", []rating{yesAt(0.3), yesAt(0.4), yesAt(0.9), noAt(0.1), noAt(0.2)}, 0.5, 0.25, 2},
		// No to posts at 0.6 and 0.7: up, nearest the current threshold.
		{"higher", []rating{yesAt(0.9), yesAt(0.95), noAt(0.6), noAt(0.7), noAt(0.1)}, 0.5, 0.8, 2},
		// Ratings that no threshold separates: the least wrong, and only
		// if it beats the current one.
		// From 0.8, three ratings disagree; 0.4 and 0.65 both leave two,
		// and 0.65 is nearer.
		{"noisy", []rating{yesAt(0.6), noAt(0.6), yesAt(0.7), noAt(0.2), yesAt(0.1)}, 0.8, 0.65, 3},
		{"noisy, already least wrong", []rating{yesAt(0.6), noAt(0.6), yesAt(0.7), noAt(0.2), yesAt(0.1)}, 0.65, 0, 2},
		// The floor keeps a threshold from chasing answers near zero.
		{"bounded", []rating{yesAt(0.01), yesAt(0.02), noAt(0.001), noAt(0.005)}, 0.5, 0, 2},
	}
	for _, c := range cases {
		got := tune(c.rs, "mainly", c.current)
		if got.Disagree != c.disagree || got.Rated != len(c.rs) || got.Yes+got.No != len(c.rs) {
			t.Errorf("%s: %+v, want %d disagreements", c.name, got, c.disagree)
		}
		switch {
		case c.want == 0 && got.Suggested != nil:
			t.Errorf("%s: suggested %v, want none", c.name, *got.Suggested)
		case c.want != 0 && (got.Suggested == nil || *got.Suggested != c.want):
			t.Errorf("%s: suggested %v, want %v", c.name, got.Suggested, c.want)
		case got.Suggested != nil && disagreements(c.rs, "mainly", *got.Suggested) >= got.Disagree:
			t.Errorf("%s: suggestion is no better", c.name)
		}
	}
}

// The filter's match decides which probability is tuned.
func TestTuneByMatch(t *testing.T) {
	rs := []rating{
		{yes: true, pMentions: 0.9, pMainly: 0.2}, {yes: true, pMentions: 0.8, pMainly: 0.3},
		{yes: false, pMentions: 0.3, pMainly: 0.1}, {yes: false, pMentions: 0.1, pMainly: 0.05},
	}
	if r := tune(rs, "mentions", 0.5); r.Disagree != 0 || r.Suggested != nil {
		t.Errorf("mentions: %+v", r)
	}
	if r := tune(rs, "mainly", 0.5); r.Disagree != 2 || r.Suggested == nil || *r.Suggested != 0.15 {
		t.Errorf("mainly: %+v", r)
	}
}
