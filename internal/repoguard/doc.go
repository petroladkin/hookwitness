// Package repoguard holds tests that guard the repository itself rather than
// any code: no private network addresses or home-directory paths in tracked
// files, a license notice for every module compiled into the binary, and
// documentation whose Ukrainian twins, links and images stay in step.
//
// Exemptions for the private-string scan: put the marker guard:allow on the
// line, or guard:allow-file in the first line of a file (large fixtures).
package repoguard
