// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package format

import (
	"iter"
	"math"
)

// Splits fields into sub-slices based on their length to isolate fields or
// groups of fields that are significantly longer than others in the group.
//
// The algorithm itself and the constants used in this function are from gofmt:
// https://github.com/golang/go/blob/go1.23.0/src/go/printer/nodes.go#L126
func splitSegmentedFields(fields []segmentedField) iter.Seq[[]segmentedField] {
	return func(yield func([]segmentedField) bool) {
		const r = 2.5
		const smallSize = 40
		var count, lower, size int
		var lnsum float64
		for i := 0; i < len(fields); i++ {
			f := fields[i]
			prevSize := size
			size = len(f.typeName) + len(f.fieldName)
			if size > 0 && prevSize > 0 && count > 0 && (prevSize > smallSize || size > smallSize) {
				mean := math.Exp(lnsum / float64(count))
				ratio := float64(size) / mean
				if r*ratio <= 1 || r <= ratio {
					// split the group
					yield(fields[lower:i])
					lower = i
					count = 0
					lnsum = 0
				}
			}
			if size > 0 {
				count++
				lnsum += math.Log(float64(size))
			}
		}
		yield(fields[lower:])
	}
}
