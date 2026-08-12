// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"fmt"
	"regexp"
	"strings"
)

// Labels are free-form, control-plane-attested key=value metadata a node's
// identity carries (e.g. region="us-east-1", team="platform"). Cloud
// providers, on-prem operators, and countries all name things differently,
// so SAM imposes no taxonomy or hierarchy on keys or values: composition
// (e.g. attesting both a precise and a coarser value so a coarser
// requirement also matches) is entirely up to the operator. Matching is
// exact and case-sensitive on both key and value (see LabelCheck).
//
// Well-known keys (e.g. LabelRegion) are just conventions; SAM does not
// validate or interpret their values beyond ValidateLabelValue.

// labelKeySyntax bounds label keys to a safe, portable charset.
var labelKeySyntax = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,63}$`)

// maxLabelValueLen bounds label values defensively; well within any
// realistic cloud region/zone or on-prem naming convention.
const maxLabelValueLen = 255

// ValidateLabelKey checks that a label key is well-formed: 1-63 characters
// from [a-zA-Z0-9_.-].
func ValidateLabelKey(key string) error {
	if !labelKeySyntax.MatchString(key) {
		return fmt.Errorf("invalid label key %q: must be 1-63 chars of [a-zA-Z0-9_.-]", key)
	}
	return nil
}

// ValidateLabelValue checks that a label value is well-formed: non-empty,
// bounded length, and free of characters that collide with the wire-format
// separators (comma-separated key=value pairs) or control characters.
func ValidateLabelValue(value string) error {
	if value == "" {
		return fmt.Errorf("label value must not be empty")
	}
	if len(value) > maxLabelValueLen {
		return fmt.Errorf("label value %q exceeds %d characters", value, maxLabelValueLen)
	}
	if strings.ContainsAny(value, ",=\n\r\t") {
		return fmt.Errorf("label value %q must not contain ',', '=', or control characters", value)
	}
	return nil
}

// ValidateLabels checks every key and value in a label set.
func ValidateLabels(labels map[string]string) error {
	for k, v := range labels {
		if err := ValidateLabelKey(k); err != nil {
			return err
		}
		if err := ValidateLabelValue(v); err != nil {
			return err
		}
	}
	return nil
}
