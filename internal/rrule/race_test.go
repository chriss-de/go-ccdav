//go:build race

package rrule

// timeScale stretches the time limits of tests: the race detector makes
// the code several times slower.
const timeScale = 10
