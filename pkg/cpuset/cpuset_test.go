package cpuset

import "testing"

func TestParseAndString(t *testing.T) {
	cases := []struct {
		in   string
		want string
		size int
	}{
		{"", "", 0},
		{"0", "0", 1},
		{"0-3", "0-3", 4},
		{"0,1", "0,1", 2},
		{"0,1,2", "0-2", 3},
		{"3,1,0,2", "0-3", 4},
		{"0-3,8", "0-3,8", 5},
		{"0-1,4-7,12", "0,1,4-7,12", 7},
		{" 0 - 2 , 5 ", "0-2,5", 4},
		// Overlapping ranges collapse rather than double-count.
		{"0-4,2-6", "0-6", 7},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		if got.Size() != tc.size {
			t.Errorf("Parse(%q).Size() = %d, want %d", tc.in, got.Size(), tc.size)
		}
		if s := got.String(); s != tc.want {
			t.Errorf("Parse(%q).String() = %q, want %q", tc.in, s, tc.want)
		}
		// Everything the kernel accepts must survive a round trip, or the agent
		// will write back a set it did not mean.
		again, err := Parse(got.String())
		if err != nil || !again.Equals(got) {
			t.Errorf("round trip of %q failed: %v", tc.in, err)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{"a", "0-", "-3", "5-2", "0,,x"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should have failed", in)
		}
	}
}

func TestSetAlgebra(t *testing.T) {
	a := MustParse("0-7")
	b := MustParse("4-11")

	if got := a.Union(b).String(); got != "0-11" {
		t.Errorf("union = %q", got)
	}
	if got := a.Intersection(b).String(); got != "4-7" {
		t.Errorf("intersection = %q", got)
	}
	if got := a.Difference(b).String(); got != "0-3" {
		t.Errorf("difference = %q", got)
	}
	if !a.Difference(a).IsEmpty() {
		t.Error("a - a should be empty")
	}
}

func TestTake(t *testing.T) {
	all := MustParse("0-7")

	taken, rest := all.Take(3)
	if taken.String() != "0-2" || rest.String() != "3-7" {
		t.Errorf("Take(3) = %q / %q", taken, rest)
	}

	// Asking for more than exists yields everything rather than an error, so
	// callers see an undersized set and can fall back.
	taken, rest = all.Take(99)
	if taken.Size() != 8 || !rest.IsEmpty() {
		t.Errorf("Take(99) = %q / %q", taken, rest)
	}

	taken, rest = all.Take(0)
	if !taken.IsEmpty() || rest.Size() != 8 {
		t.Errorf("Take(0) = %q / %q", taken, rest)
	}
}

func TestTakeFromPrefersZone(t *testing.T) {
	free := MustParse("0-3,8-11")
	zone := MustParse("8-15")

	// A NUMA-bound request must be satisfied entirely inside the zone when the
	// zone can hold it.
	taken, rest := free.TakeFrom(3, zone)
	if taken.String() != "8-10" {
		t.Errorf("TakeFrom stayed outside the zone: %q", taken)
	}
	if rest.Contains(8) || rest.Contains(9) || rest.Contains(10) {
		t.Errorf("remainder still holds taken cpus: %q", rest)
	}

	// When the zone is too small it spills over rather than failing, because a
	// degraded placement beats an unschedulable pod.
	taken, _ = free.TakeFrom(6, zone)
	if taken.Size() != 6 {
		t.Errorf("TakeFrom(6) size = %d, want 6", taken.Size())
	}
	if taken.Intersection(zone).Size() != 4 {
		t.Errorf("spill-over should still exhaust the zone first: %q", taken)
	}
}
