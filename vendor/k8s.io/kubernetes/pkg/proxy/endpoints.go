/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proxy

import (
	"net"
	"strconv"
	"sync"
	"time"

	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/proxy/metrics"
)

var supportedEndpointSliceAddressTypes = sets.New[string](
	string(discovery.AddressTypeIPv4),
	string(discovery.AddressTypeIPv6),
)

// BaseEndpointInfo contains base information that defines an endpoint.
// This could be used directly by proxier while processing endpoints,
// or can be used for constructing a more specific EndpointInfo struct
// defined by the proxier if needed.
type BaseEndpointInfo struct {
	// Cache this values to improve performance
	ip   string
	port int
	// endpoint is the same as net.JoinHostPort(ip,port)
	endpoint string

	// isLocal indicates whether the endpoint is running on same host as kube-proxy.
	isLocal bool

	// ready indicates whether this endpoint is ready and NOT terminating, unless
	// PublishNotReadyAddresses is set on the service, in which case it will just
	// always be true.
	ready bool
	// serving indicates whether this endpoint is ready regardless of its terminating state.
	// For pods this is true if it has a ready status regardless of its deletion timestamp.
	serving bool
	// terminating indicates whether this endpoint is terminating.
	// For pods this is true if it has a non-nil deletion timestamp.
	terminating bool

	// zoneHints represent the zone hints for the endpoint. This is based on
	// endpoint.hints.forZones[*].name in the EndpointSlice API.
	zoneHints sets.Set[string]
}

var _ Endpoint = &BaseEndpointInfo{}

// String is part of proxy.Endpoint interface.
func (info *BaseEndpointInfo) String() string {
	return info.endpoint
}

// IP returns just the IP part of the endpoint, it's a part of proxy.Endpoint interface.
func (info *BaseEndpointInfo) IP() string {
	return info.ip
}

// Port returns just the Port part of the endpoint.
func (info *BaseEndpointInfo) Port() int {
	return info.port
}

// IsLocal is part of proxy.Endpoint interface.
func (info *BaseEndpointInfo) IsLocal() bool {
	return info.isLocal
}

// IsReady returns true if an endpoint is ready and not terminating.
func (info *BaseEndpointInfo) IsReady() bool {
	return info.ready
}

// IsServing returns true if an endpoint is ready, regardless of if the
// endpoint is terminating.
func (info *BaseEndpointInfo) IsServing() bool {
	return info.serving
}

// IsTerminating retruns true if an endpoint is terminating. For pods,
// that is any pod with a deletion timestamp.
func (info *BaseEndpointInfo) IsTerminating() bool {
	return info.terminating
}

// ZoneHints returns the zone hint for the endpoint.
func (info *BaseEndpointInfo) ZoneHints() sets.Set[string] {
	return info.zoneHints
}

func newBaseEndpointInfo(ip string, port int, isLocal, ready, serving, terminating bool, zoneHints sets.Set[string]) *BaseEndpointInfo {
	return &BaseEndpointInfo{
		ip:          ip,
		port:        port,
		endpoint:    net.JoinHostPort(ip, strconv.Itoa(port)),
		isLocal:     isLocal,
		ready:       ready,
		serving:     serving,
		terminating: terminating,
		zoneHints:   zoneHints,
	}
}

type makeEndpointFunc func(info *BaseEndpointInfo, svcPortName *ServicePortName) Endpoint

// This handler is invoked by the apply function on every change. This function should not modify the
// EndpointsMap's but just use the changes for any Proxier specific cleanup.
type processEndpointsMapChangeFunc func(oldEndpointsMap, newEndpointsMap EndpointsMap)

// EndpointsChangeTracker carries state about uncommitted changes to an arbitrary number of
// Endpoints, keyed by their namespace and name.
type EndpointsChangeTracker struct {
	// lock protects lastChangeTriggerTimes
	lock sync.Mutex

	processEndpointsMapChange processEndpointsMapChangeFunc
	// endpointSliceCache holds a simplified version of endpoint slices.
	endpointSliceCache *EndpointSliceCache
	// Map from the Endpoints namespaced-name to the times of the triggers that caused the endpoints
	// object to change. Used to calculate the network-programming-latency.
	lastChangeTriggerTimes map[types.NamespacedName][]time.Time
	// record the time when the endpointsChangeTracker was created so we can ignore the endpoints
	// that were generated before, because we can't estimate the network-programming-latency on those.
	// This is specially problematic on restarts, because we process all the endpoints that may have been
	// created hours or days before.
	trackerStartTime time.Time
}

// NewEndpointsChangeTracker initializes an EndpointsChangeTracker
func NewEndpointsChangeTracker(hostname string, makeEndpointInfo makeEndpointFunc, ipFamily v1.IPFamily, recorder events.EventRecorder, processEndpointsMapChange processEndpointsMapChangeFunc) *EndpointsChangeTracker {
	return &EndpointsChangeTracker{
		lastChangeTriggerTimes:    make(map[types.NamespacedName][]time.Time),
		trackerStartTime:          time.Now(),
		processEndpointsMapChange: processEndpointsMapChange,
		endpointSliceCache:        NewEndpointSliceCache(hostname, ipFamily, recorder, makeEndpointInfo),
	}
}

// EndpointSliceUpdate updates given service's endpoints change map based on the <previous, current> endpoints pair.
// It returns true if items changed, otherwise return false. Will add/update/delete items of EndpointsChangeTracker.
// If removeSlice is true, slice will be removed, otherwise it will be added or updated.
func (ect *EndpointsChangeTracker) EndpointSliceUpdate(endpointSlice *discovery.EndpointSlice, removeSlice bool) bool {
	if !supportedEndpointSliceAddressTypes.Has(string(endpointSlice.AddressType)) {
		klog.V(4).InfoS("EndpointSlice address type not supported by kube-proxy", "addressType", endpointSlice.AddressType)
		return false
	}

	// This should never happen
	if endpointSlice == nil {
		klog.ErrorS(nil, "Nil endpointSlice passed to EndpointSliceUpdate")
		return false
	}

	namespacedName, _, err := endpointSliceCacheKeys(endpointSlice)
	if err != nil {
		klog.InfoS("Error getting endpoint slice cache keys", "err", err)
		return false
	}

	metrics.EndpointChangesTotal.Inc()

	ect.lock.Lock()
	defer ect.lock.Unlock()

	changeNeeded := ect.endpointSliceCache.updatePending(endpointSlice, removeSlice)

	if changeNeeded {
		metrics.EndpointChangesPending.Inc()
		// In case of Endpoints deletion, the LastChangeTriggerTime annotation is
		// by-definition coming from the time of last update, which is not what
		// we want to measure. So we simply ignore it in this cases.
		// TODO(wojtek-t, robscott): Address the problem for EndpointSlice deletion
		// when other EndpointSlice for that service still exist.
		if removeSlice {
			delete(ect.lastChangeTriggerTimes, namespacedName)
		} else if t := getLastChangeTriggerTime(endpointSlice.Annotations); !t.IsZero() && t.After(ect.trackerStartTime) {
			ect.lastChangeTriggerTimes[namespacedName] =
				append(ect.lastChangeTriggerTimes[namespacedName], t)
		}
	}

	return changeNeeded
}

// checkoutChanges returns a map of pending endpointsChanges and marks them as
// applied.
func (ect *EndpointsChangeTracker) checkoutChanges() map[types.NamespacedName]*endpointsChange {
	metrics.EndpointChangesPending.Set(0)

	return ect.endpointSliceCache.checkoutChanges()
}

// checkoutTriggerTimes applies the locally cached trigger times to a map of
// trigger times that have been passed in and empties the local cache.
func (ect *EndpointsChangeTracker) checkoutTriggerTimes(lastChangeTriggerTimes *map[types.NamespacedName][]time.Time) {
	ect.lock.Lock()
	defer ect.lock.Unlock()

	for k, v := range ect.lastChangeTriggerTimes {
		prev, ok := (*lastChangeTriggerTimes)[k]
		if !ok {
			(*lastChangeTriggerTimes)[k] = v
		} else {
			(*lastChangeTriggerTimes)[k] = append(prev, v...)
		}
	}
	ect.lastChangeTriggerTimes = make(map[types.NamespacedName][]time.Time)
}

// getLastChangeTriggerTime returns the time.Time value of the
// EndpointsLastChangeTriggerTime annotation stored in the given endpoints
// object or the "zero" time if the annotation wasn't set or was set
// incorrectly.
func getLastChangeTriggerTime(annotations map[string]string) time.Time {
	// TODO(#81360): ignore case when Endpoint is deleted.
	if _, ok := annotations[v1.EndpointsLastChangeTriggerTime]; !ok {
		// It's possible that the Endpoints object won't have the
		// EndpointsLastChangeTriggerTime annotation set. In that case return
		// the 'zero value', which is ignored in the upstream code.
		return time.Time{}
	}
	val, err := time.Parse(time.RFC3339Nano, annotations[v1.EndpointsLastChangeTriggerTime])
	if err != nil {
		klog.ErrorS(err, "Error while parsing EndpointsLastChangeTriggerTimeAnnotation",
			"value", annotations[v1.EndpointsLastChangeTriggerTime])
		// In case of error val = time.Zero, which is ignored in the upstream code.
	}
	return val
}

// endpointsChange contains all changes to endpoints that happened since proxy
// rules were synced.  For a single object, changes are accumulated, i.e.
// previous is state from before applying the changes, current is state after
// applying the changes.
type endpointsChange struct {
	previous EndpointsMap
	current  EndpointsMap
}

// UpdateEndpointsMapResult is the updated results after applying endpoints changes.
type UpdateEndpointsMapResult struct {
	// UpdatedServices lists the names of all services with added/updated/deleted
	// endpoints since the last Update.
	UpdatedServices sets.Set[types.NamespacedName]

	// DeletedUDPEndpoints identifies UDP endpoints that have just been deleted.
	// Existing conntrack NAT entries pointing to these endpoints must be deleted to
	// ensure that no further traffic for the Service gets delivered to them.
	DeletedUDPEndpoints []ServiceEndpoint

	// NewlyActiveUDPServices identifies UDP Services that have just gone from 0 to
	// non-0 endpoints. Existing conntrack entries caching the fact that these
	// services are black holes must be deleted to ensure that traffic can immediately
	// begin flowing to the new endpoints.
	NewlyActiveUDPServices []ServicePortName

	// List of the trigger times for all endpoints objects that changed. It's used to export the
	// network programming latency.
	// NOTE(oxddr): this can be simplified to []time.Time if memory consumption becomes an issue.
	LastChangeTriggerTimes map[types.NamespacedName][]time.Time
}

// EndpointsMap maps a service name to a list of all its Endpoints.
type EndpointsMap map[ServicePortName][]Endpoint

// Update updates em based on the changes in ect, returns information about the diff since
// the last Update, triggers processEndpointsMapChange on every change, and clears the
// changes map.
func (em EndpointsMap) Update(ect *EndpointsChangeTracker) UpdateEndpointsMapResult {
	result := UpdateEndpointsMapResult{
		UpdatedServices:        sets.New[types.NamespacedName](),
		DeletedUDPEndpoints:    make([]ServiceEndpoint, 0),
		NewlyActiveUDPServices: make([]ServicePortName, 0),
		LastChangeTriggerTimes: make(map[types.NamespacedName][]time.Time),
	}
	if ect == nil {
		return result
	}

	changes := ect.checkoutChanges()
	for nn, change := range changes {
		if ect.processEndpointsMapChange != nil {
			ect.processEndpointsMapChange(change.previous, change.current)
		}
		result.UpdatedServices.Insert(nn)

		em.unmerge(change.previous)
		em.merge(change.current)
		detectStaleConntrackEntries(change.previous, change.current, &result.DeletedUDPEndpoints, &result.NewlyActiveUDPServices)
	}
	ect.checkoutTriggerTimes(&result.LastChangeTriggerTimes)

	return result
}

// Merge ensures that the current EndpointsMap contains all <service, endpoints> pairs from the EndpointsMap passed in.
func (em EndpointsMap) merge(other EndpointsMap) {
	for svcPortName := range other {
		em[svcPortName] = other[svcPortName]
	}
}

// Unmerge removes the <service, endpoints> pairs from the current EndpointsMap which are contained in the EndpointsMap passed in.
func (em EndpointsMap) unmerge(other EndpointsMap) {
	for svcPortName := range other {
		delete(em, svcPortName)
	}
}

// getLocalEndpointIPs returns endpoints IPs if given endpoint is local - local means the endpoint is running in same host as kube-proxy.
func (em EndpointsMap) getLocalReadyEndpointIPs() map[types.NamespacedName]sets.Set[string] {
	localIPs := make(map[types.NamespacedName]sets.Set[string])
	for svcPortName, epList := range em {
		for _, ep := range epList {
			// Only add ready endpoints for health checking. Terminating endpoints may still serve traffic
			// but the health check signal should fail if there are only terminating endpoints on a node.
			if !ep.IsReady() {
				continue
			}

			if ep.IsLocal() {
				nsn := svcPortName.NamespacedName
				if localIPs[nsn] == nil {
					localIPs[nsn] = sets.New[string]()
				}
				localIPs[nsn].Insert(ep.IP())
			}
		}
	}
	return localIPs
}

// LocalReadyEndpoints returns a map of Service names to the number of local ready
// endpoints for that service.
func (em EndpointsMap) LocalReadyEndpoints() map[types.NamespacedName]int {
	// TODO: If this will appear to be computationally expensive, consider
	// computing this incrementally similarly to endpointsMap.

	// (Note that we need to call getLocalEndpointIPs first to squash the data by IP,
	// because the EndpointsMap is sorted by IP+port, not just IP, and we want to
	// consider a Service pointing to 10.0.0.1:80 and 10.0.0.1:443 to have 1 endpoint,
	// not 2.)

	eps := make(map[types.NamespacedName]int)
	localIPs := em.getLocalReadyEndpointIPs()
	for nsn, ips := range localIPs {
		eps[nsn] = len(ips)
	}
	return eps
}

// detectStaleConntrackEntries detects services that may be associated with stale conntrack entries.
// (See UpdateEndpointsMapResult.DeletedUDPEndpoints and .NewlyActiveUDPServices.)
func detectStaleConntrackEntries(oldEndpointsMap, newEndpointsMap EndpointsMap, deletedUDPEndpoints *[]ServiceEndpoint, newlyActiveUDPServices *[]ServicePortName) {
	// Find the UDP endpoints that we were sending traffic to in oldEndpointsMap, but
	// are no longer sending to newEndpointsMap. The proxier should make sure that
	// conntrack does not accidentally route any new connections to them.
	for svcPortName, epList := range oldEndpointsMap {
		if svcPortName.Protocol != v1.ProtocolUDP {
			continue
		}

		for _, ep := range epList {
			// If the old endpoint wasn't Serving then there can't be stale
			// conntrack entries since there was no traffic sent to it.
			if !ep.IsServing() {
				continue
			}

			deleted := true
			// Check if the endpoint has changed, including if it went from
			// serving to not serving. If it did change stale entries for the old
			// endpoint have to be cleared.
			for i := range newEndpointsMap[svcPortName] {
				if newEndpointsMap[svcPortName][i].String() == ep.String() &&
					newEndpointsMap[svcPortName][i].IsServing() == ep.IsServing() {
					deleted = false
					break
				}
			}
			if deleted {
				klog.V(4).InfoS("Deleted endpoint may have stale conntrack entries", "portName", svcPortName, "endpoint", ep)
				*deletedUDPEndpoints = append(*deletedUDPEndpoints, ServiceEndpoint{Endpoint: ep.String(), ServicePortName: svcPortName})
			}
		}
	}

	// Detect services that have gone from 0 to non-0 ready endpoints. If there were
	// previously 0 endpoints, but someone tried to connect to it, then a conntrack
	// entry may have been created blackholing traffic to that IP, which should be
	// deleted now.
	for svcPortName, epList := range newEndpointsMap {
		if svcPortName.Protocol != v1.ProtocolUDP {
			continue
		}

		epServing := 0
		for _, ep := range epList {
			if ep.IsServing() {
				epServing++
			}
		}

		oldEpServing := 0
		for _, ep := range oldEndpointsMap[svcPortName] {
			if ep.IsServing() {
				oldEpServing++
			}
		}

		if epServing > 0 && oldEpServing == 0 {
			*newlyActiveUDPServices = append(*newlyActiveUDPServices, svcPortName)
		}
	}
}
