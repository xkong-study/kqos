// Package cpuset parses and renders the Linux cpuset list format ("0-3,8,12-15")
// and provides the set algebra the CPU pool manager needs.
package cpuset

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CPUSet is an immutable set of CPU ids.
type CPUSet struct {
	elems map[int]struct{}
}

// New builds a CPUSet from explicit ids.
func New(ids ...int) CPUSet {
	s := CPUSet{elems: make(map[int]struct{}, len(ids))}
	for _, id := range ids {
		s.elems[id] = struct{}{}
	}
	return s
}

// NewRange builds the set [start, end).
func NewRange(start, end int) CPUSet {
	s := CPUSet{elems: make(map[int]struct{})}
	for i := start; i < end; i++ {
		s.elems[i] = struct{}{}
	}
	return s
}

// Parse reads the kernel's cpuset list format. An empty or whitespace-only
// string yields the empty set rather than an error, matching how the kernel
// renders an unconstrained cpuset in some configurations.
func Parse(s string) (CPUSet, error) {
	out := CPUSet{elems: make(map[int]struct{})}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		start, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return CPUSet{}, fmt.Errorf("cpuset %q: bad element %q: %w", s, part, err)
		}
		end := start
		if found {
			end, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return CPUSet{}, fmt.Errorf("cpuset %q: bad range end %q: %w", s, part, err)
			}
		}
		if end < start {
			return CPUSet{}, fmt.Errorf("cpuset %q: descending range %q", s, part)
		}
		for i := start; i <= end; i++ {
			out.elems[i] = struct{}{}
		}
	}
	return out, nil
}

// MustParse is Parse for constants and tests.
func MustParse(s string) CPUSet {
	c, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return c
}

// Size is the number of CPUs in the set.
func (c CPUSet) Size() int { return len(c.elems) }

// IsEmpty reports whether the set has no members.
func (c CPUSet) IsEmpty() bool { return len(c.elems) == 0 }

// Contains reports membership.
func (c CPUSet) Contains(id int) bool {
	_, ok := c.elems[id]
	return ok
}

// List returns the members in ascending order.
func (c CPUSet) List() []int {
	out := make([]int, 0, len(c.elems))
	for id := range c.elems {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// String renders the set back into the kernel's list format, collapsing runs
// into ranges so the output round-trips through Parse.
func (c CPUSet) String() string {
	ids := c.List()
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		switch j - i {
		case 0:
			fmt.Fprintf(&b, "%d", ids[i])
		case 1:
			fmt.Fprintf(&b, "%d,%d", ids[i], ids[j])
		default:
			fmt.Fprintf(&b, "%d-%d", ids[i], ids[j])
		}
		i = j + 1
	}
	return b.String()
}

// Union returns the members of either set.
func (c CPUSet) Union(other CPUSet) CPUSet {
	out := CPUSet{elems: make(map[int]struct{}, len(c.elems)+len(other.elems))}
	for id := range c.elems {
		out.elems[id] = struct{}{}
	}
	for id := range other.elems {
		out.elems[id] = struct{}{}
	}
	return out
}

// Intersection returns the members present in both sets.
func (c CPUSet) Intersection(other CPUSet) CPUSet {
	out := CPUSet{elems: make(map[int]struct{})}
	for id := range c.elems {
		if other.Contains(id) {
			out.elems[id] = struct{}{}
		}
	}
	return out
}

// Difference returns the members of c that are not in other.
func (c CPUSet) Difference(other CPUSet) CPUSet {
	out := CPUSet{elems: make(map[int]struct{})}
	for id := range c.elems {
		if !other.Contains(id) {
			out.elems[id] = struct{}{}
		}
	}
	return out
}

// Equals reports set equality.
func (c CPUSet) Equals(other CPUSet) bool {
	if len(c.elems) != len(other.elems) {
		return false
	}
	for id := range c.elems {
		if !other.Contains(id) {
			return false
		}
	}
	return true
}

// Take removes and returns n CPUs, preferring the lowest ids. It returns the
// taken set and the remainder. If fewer than n are available it takes all of
// them; callers are expected to check Size on the result.
func (c CPUSet) Take(n int) (taken, rest CPUSet) {
	ids := c.List()
	if n > len(ids) {
		n = len(ids)
	}
	if n < 0 {
		n = 0
	}
	return New(ids[:n]...), New(ids[n:]...)
}

// TakeFrom removes n CPUs preferring ids that are also in preferred, which is
// how NUMA-aware allocation keeps a dedicated pod inside one zone before
// spilling over into another.
func (c CPUSet) TakeFrom(n int, preferred CPUSet) (taken, rest CPUSet) {
	fromPreferred, _ := c.Intersection(preferred).Take(n)
	if fromPreferred.Size() < n {
		extra, _ := c.Difference(fromPreferred).Take(n - fromPreferred.Size())
		fromPreferred = fromPreferred.Union(extra)
	}
	return fromPreferred, c.Difference(fromPreferred)
}
