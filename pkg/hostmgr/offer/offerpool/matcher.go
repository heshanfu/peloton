// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package offerpool

import (
	"math"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/uber/peloton/.gen/peloton/private/hostmgr/hostsvc"

	"github.com/uber/peloton/pkg/common/constraints"
	"github.com/uber/peloton/pkg/hostmgr/summary"
)

// effectiveHostLimit is common helper function to determine effective limit on
// number of hosts.
func effectiveHostLimit(f *hostsvc.HostFilter) uint32 {
	limit := f.GetQuantity().GetMaxHosts()
	if limit == 0 {
		limit = math.MaxUint32
	}
	return limit
}

// Matcher keeps track of matched host offers for given constraints.
type Matcher struct {
	hostFilter *hostsvc.HostFilter
	evaluator  constraints.Evaluator
	// map of hostname to the host offer
	hostOffers map[string]*summary.Offer

	filterResultCounts map[string]uint32
}

// tryMatch tries to match ready unreserved offers in summary with particular
// constraint.
// If properly matched, the offers will be kept in Matcher for later return,
// otherwise they are untouched.
func (m *Matcher) tryMatch(
	hostname string,
	s summary.HostSummary) {
	result := m.tryMatchImpl(hostname, s)
	if name, ok := hostsvc.HostFilterResult_name[int32(result)]; !ok {
		log.WithField("value", result).
			Error("Unknown enum value for HostFilterResult_name")
	} else {
		m.filterResultCounts[strings.ToLower(name)]++
	}
}

func (m *Matcher) tryMatchImpl(
	hostname string,
	s summary.HostSummary) hostsvc.HostFilterResult {
	if m.HasEnoughHosts() {
		return hostsvc.HostFilterResult_MISMATCH_MAX_HOST_LIMIT
	}

	if _, exist := m.hostOffers[hostname]; exist {
		return hostsvc.HostFilterResult_MATCH
	}

	match := s.TryMatch(m.hostFilter, m.evaluator)
	log.WithFields(log.Fields{
		"host_filter": m.hostFilter,
		"host":        hostname,
		"status":      s.GetHostStatus(),
		"match":       match,
	}).Debug("Constraint matching result")

	if match.Result == hostsvc.HostFilterResult_MATCH {
		m.hostOffers[hostname] = match.Offer
	}
	return match.Result
}

// HasEnoughHosts returns whether this instance has matched enough hosts based
// on input HostLimit.
func (m *Matcher) HasEnoughHosts() bool {
	return uint32(len(m.hostOffers)) >= effectiveHostLimit(m.hostFilter)
}

// getHostOffers returns all hostOffers from matcher and clears cached result.
func (m *Matcher) getHostOffers() (map[string]*summary.Offer, map[string]uint32) {
	result := make(map[string]*summary.Offer)
	resultCount := make(map[string]uint32)
	// swap
	result, m.hostOffers = m.hostOffers, result
	resultCount, m.filterResultCounts = m.filterResultCounts, resultCount
	return result, resultCount
}

// NewMatcher returns a new instance of Matcher.
func NewMatcher(
	hostFilter *hostsvc.HostFilter,
	evaluator constraints.Evaluator,
) *Matcher {
	return &Matcher{
		hostFilter:         hostFilter,
		evaluator:          evaluator,
		hostOffers:         make(map[string]*summary.Offer),
		filterResultCounts: make(map[string]uint32),
	}
}
