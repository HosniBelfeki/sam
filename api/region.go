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
	"sort"
	"strings"
)

// Region claims are hierarchical: CONTINENT[-COUNTRY[-ZONE]].
//
//   - CONTINENT: two-letter continent code (seven-continent model, as used
//     by geolocation databases): AF, AN, AS, EU, NA, OC, SA.
//   - COUNTRY: ISO 3166-1 alpha-2 code, validated to belong to the claimed
//     continent (transcontinental countries follow common
//     geolocation-database convention, e.g. RU->EU, TR->AS, CY->EU).
//   - ZONE: reserved extension for ISO 3166-2 subdivision codes (the part
//     after the country prefix, e.g. "BY" in DE-BY); syntax-validated only.
//
// Examples: "EU", "EU-DE", "EU-DE-BY", "NA-US", "NA-US-CA".
//
// Matching is hierarchical and fail-closed: requiring "EU" matches a
// provider claiming "EU-DE"; requiring "EU-DE" does NOT match a provider
// claiming only "EU" (a coarser claim cannot guarantee the finer scope).
// The set is closed and validated: anything else is rejected at the boundary
// that receives it — node startup for --region, HTTP 400 for
// X-Sam-Required-Region, and dropped announcements on the gossip layer.
var continentNames = map[string]string{
	"AF": "Africa",
	"AN": "Antarctica",
	"AS": "Asia",
	"EU": "Europe",
	"NA": "North America",
	"OC": "Oceania",
	"SA": "South America",
}

// countryContinent maps ISO 3166-1 alpha-2 codes to their continent.
var countryContinent = map[string]string{}

func init() {
	byContinent := map[string]string{
		"AF": "AO BF BI BJ BW CD CF CG CI CM CV DJ DZ EG EH ER ET GA GH GM GN GQ GW KE KM LR LS LY MA MG ML MR MU MW MZ NA NE NG RE RW SC SD SH SL SN SO SS ST SZ TD TG TN TZ UG YT ZA ZM ZW",
		"AN": "AQ BV GS HM TF",
		"AS": "AE AF AM AZ BD BH BN BT CN GE HK ID IL IN IO IQ IR JO JP KG KH KP KR KW KZ LA LB LK MM MN MO MV MY NP OM PH PK PS QA SA SG SY TH TJ TL TM TR TW UZ VN YE",
		"EU": "AD AL AT AX BA BE BG BY CH CY CZ DE DK EE ES FI FO FR GB GG GI GR HR HU IE IM IS IT JE LI LT LU LV MC MD ME MK MT NL NO PL PT RO RS RU SE SI SJ SK SM UA VA",
		"NA": "AG AI AW BB BL BM BQ BS BZ CA CR CU CW DM DO GD GL GP GT HN HT JM KN KY LC MF MQ MS MX NI PA PM PR SV SX TC TT UM US VC VG VI",
		"OC": "AS AU CC CK CX FJ FM GU KI MH MP NC NF NR NU NZ PF PG PN PW SB TK TO TV VU WF WS",
		"SA": "AR BO BR CL CO EC FK GF GY PE PY SR UY VE",
	}
	for continent, list := range byContinent {
		for _, cc := range strings.Fields(list) {
			countryContinent[cc] = continent
		}
	}
}

// zoneSyntax is the ISO 3166-2 subdivision code part (after the country
// prefix): one to three letters or digits.
var zoneSyntax = regexp.MustCompile(`^[A-Z0-9]{1,3}$`)

// NormalizeRegion canonicalizes a region claim for comparison: uppercase,
// surrounding whitespace ignored. Every boundary that emits or matches a
// region value must apply it.
func NormalizeRegion(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// ValidateRegion checks that s (after normalization) is a well-formed region
// claim per the CONTINENT[-COUNTRY[-ZONE]] hierarchy. The empty string is
// not valid: callers decide whether an absent claim is allowed.
func ValidateRegion(s string) error {
	parts := strings.Split(NormalizeRegion(s), "-")
	if len(parts) > 3 {
		return fmt.Errorf("invalid region %q: at most CONTINENT-COUNTRY-ZONE", s)
	}
	if _, ok := continentNames[parts[0]]; !ok {
		return fmt.Errorf("invalid region %q: continent must be one of %s", s, strings.Join(ContinentCodes(), ", "))
	}
	if len(parts) >= 2 {
		continent, ok := countryContinent[parts[1]]
		if !ok {
			return fmt.Errorf("invalid region %q: %q is not an ISO 3166-1 alpha-2 country code", s, parts[1])
		}
		if continent != parts[0] {
			return fmt.Errorf("invalid region %q: country %s belongs to continent %s", s, parts[1], continent)
		}
	}
	if len(parts) == 3 && !zoneSyntax.MatchString(parts[2]) {
		return fmt.Errorf("invalid region %q: zone must be 1-3 letters or digits (ISO 3166-2)", s)
	}
	return nil
}

// RegionMatches reports whether a provider's claimed region satisfies a
// required region: exact match or the claim is a finer scope within the
// requirement. A claim coarser than the requirement never matches.
// Both values must be in canonical form (NormalizeRegion).
func RegionMatches(required, claimed string) bool {
	return claimed == required || strings.HasPrefix(claimed, required+"-")
}

// RegionPrefixes returns the hierarchy prefix closure of a region claim in
// canonical form, coarsest first: "EU-DE-BY" -> ["EU", "EU-DE", "EU-DE-BY"].
// Materializing every level lets policies match a requirement with a single
// exact fact lookup (see FactRegion) while keeping RegionMatches semantics:
// a finer claim carries its coarser prefixes, a coarser claim never gains
// finer ones. The empty string returns nil.
func RegionPrefixes(s string) []string {
	s = NormalizeRegion(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "-")
	out := make([]string, 0, len(parts))
	for i := range parts {
		out = append(out, strings.Join(parts[:i+1], "-"))
	}
	return out
}

// ContinentCodes returns the valid continent codes, sorted.
func ContinentCodes() []string {
	codes := make([]string, 0, len(continentNames))
	for c := range continentNames {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	return codes
}
