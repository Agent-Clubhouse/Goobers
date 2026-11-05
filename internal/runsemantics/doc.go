// Package runsemantics owns shared workflow decisions and data contracts used
// by the local runner and Temporal engine. It has no executor construction or
// stage dispatch. Keeping these rules together preserves identical result,
// evidence, retry and journal semantics across execution substrates.
package runsemantics
