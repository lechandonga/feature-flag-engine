package flag

import "strings"

// Rejection describes why a configuration was refused.
type Rejection struct {
	Kind   error  // one of the sentinel dependency/config errors
	Detail string // human-readable, includes the offending flag chain
}

func (r *Rejection) Error() string { return r.Kind.Error() + ": " + r.Detail }

// CheckDependencies validates prerequisite declarations.
//
// Three distinguishable rejection kinds can be returned:
//
//  1. ErrMissingDependency      – a prerequisite key does not exist;
//  2. ErrCircularDependency     – the prerequisite graph contains a cycle
//     (a flag declaring itself is a 1-node cycle);
//  3. ErrDependencyContradiction – statically unsatisfiable configuration:
//     duplicate prerequisite entries, or a flag
//     whose prerequisite can never evaluate ON
//     (the prerequisite flag is permanently off).
//
// Only fully valid configs are returned by ParseConfig, so a rejected
// refresh never replaces the last known-good snapshot.
func (c *Config) CheckDependencies() *Rejection {
	// Pass 1: existence + duplicate + static contradiction.
	for key, f := range c.Flags {
		seen := make(map[string]struct{}, len(f.Prerequisites))
		for _, pre := range f.Prerequisites {
			target, ok := c.Flags[pre]
			if !ok {
				return &Rejection{Kind: ErrMissingDependency,
					Detail: "flag " + key + " requires missing flag " + pre}
			}
			if pre == key {
				return &Rejection{Kind: ErrCircularDependency,
					Detail: "flag " + key + " declares itself as a prerequisite"}
			}
			if _, dup := seen[pre]; dup {
				return &Rejection{Kind: ErrDependencyContradiction,
					Detail: "flag " + key + " lists prerequisite " + pre + " more than once"}
			}
			seen[pre] = struct{}{}
			if isPermanentlyOff(target) {
				return &Rejection{Kind: ErrDependencyContradiction,
					Detail: "flag " + key + " requires " + pre + " which can never evaluate ON (permanently off)"}
			}
		}
	}

	// Pass 2: cycle detection via iterative DFS with a white/grey/black
	// colouring, recording the offending chain in the error detail.
	const white, grey, black = 0, 1, 2
	color := map[string]int{}
	for start := range c.Flags {
		if color[start] != white {
			continue
		}
		type frame struct {
			key string
			idx int
		}
		stack := []frame{{start, 0}}
		path := []string{start}
		color[start] = grey
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			f := c.Flags[top.key]
			if top.idx < len(f.Prerequisites) {
				next := f.Prerequisites[top.idx]
				top.idx++
				switch color[next] {
				case grey:
					// Extract the cycle slice from the current path.
					i := 0
					for path[i] != next {
						i++
					}
					chain := append(append([]string{}, path[i:]...), next)
					return &Rejection{Kind: ErrCircularDependency,
						Detail: "dependency cycle: " + strings.Join(chain, " -> ")}
				case white:
					color[next] = grey
					stack = append(stack, frame{next, 0})
					path = append(path, next)
				}
			} else {
				color[top.key] = black
				stack = stack[:len(stack)-1]
				path = path[:len(path)-1]
			}
		}
	}
	return nil
}

// isPermanentlyOff reports whether a flag has no possible ON outcome.
// The master switch short-circuits evaluation before rules are consulted,
// so an on:false flag can never evaluate ON regardless of its rules.
func isPermanentlyOff(f *Flag) bool {
	return !f.On
}
