//go:build !race

package graph

// raceEnabled reports whether the binary was built with -race. Timing-based
// test assertions relax under race instrumentation, which slows execution
// significantly and isn't representative of normal performance.
const raceEnabled = false
