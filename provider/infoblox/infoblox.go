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

package infoblox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	transform "github.com/StackExchange/dnscontrol/pkg/transform"
	ibclient "github.com/infobloxopen/infoblox-go-client"
	"github.com/sirupsen/logrus"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

const (
	// provider specific key to track if PTR record was already created or not for A records
	providerSpecificInfobloxPtrRecord = "infoblox-ptr-record-exists"
)

// InfobloxConfig clarifies the method signature
type InfobloxConfig struct {
	DomainFilter  endpoint.DomainFilter
	ZoneIDFilter  provider.ZoneIDFilter
	Host          string
	Port          int
	Username      string
	Password      string
	Version       string
	SSLVerify     bool
	DryRun        bool
	View          string
	MaxResults    int
	FQDNRexEx     string
	CreatePTR     bool
	CacheDuration int
}

// InfobloxProvider implements the DNS provider for Infoblox.
type InfobloxProvider struct {
	provider.BaseProvider
	client        ibclient.IBConnector
	domainFilter  endpoint.DomainFilter
	zoneIDFilter  provider.ZoneIDFilter
	view          string
	dryRun        bool
	fqdnRegEx     string
	createPTR     bool
	cacheDuration int
}

type infobloxRecordSet struct {
	obj ibclient.IBObject
	res interface{}
}

// ExtendedRequestBuilder implements a HttpRequestBuilder which sets
// additional query parameter on all get requests
type ExtendedRequestBuilder struct {
	fqdnRegEx  string
	maxResults int
	ibclient.WapiRequestBuilder
}

// NewExtendedRequestBuilder returns a ExtendedRequestBuilder which adds
// _max_results query parameter to all GET requests
func NewExtendedRequestBuilder(maxResults int, fqdnRegEx string) *ExtendedRequestBuilder {
	return &ExtendedRequestBuilder{
		fqdnRegEx:  fqdnRegEx,
		maxResults: maxResults,
	}
}

// BuildRequest prepares the api request. it uses BuildRequest of
// WapiRequestBuilder and then add the _max_requests parameter
func (mrb *ExtendedRequestBuilder) BuildRequest(t ibclient.RequestType, obj ibclient.IBObject, ref string, queryParams ibclient.QueryParams) (req *http.Request, err error) {
	req, err = mrb.WapiRequestBuilder.BuildRequest(t, obj, ref, queryParams)
	if req.Method == "GET" {
		query := req.URL.Query()
		if mrb.maxResults > 0 {
			query.Set("_max_results", strconv.Itoa(mrb.maxResults))
		}
		_, ok := obj.(*ibclient.ZoneAuth)
		if ok && t == ibclient.GET && mrb.fqdnRegEx != "" {
			query.Set("fqdn~", mrb.fqdnRegEx)
		}
		req.URL.RawQuery = query.Encode()
	}
	return
}

// NewInfobloxProvider creates a new Infoblox provider.
func NewInfobloxProvider(infobloxConfig InfobloxConfig) (*InfobloxProvider, error) {
	hostConfig := ibclient.HostConfig{
		Host:     infobloxConfig.Host,
		Port:     strconv.Itoa(infobloxConfig.Port),
		Username: infobloxConfig.Username,
		Password: infobloxConfig.Password,
		Version:  infobloxConfig.Version,
	}

	httpPoolConnections := lookupEnvAtoi("EXTERNAL_DNS_INFOBLOX_HTTP_POOL_CONNECTIONS", 10)
	httpRequestTimeout := lookupEnvAtoi("EXTERNAL_DNS_INFOBLOX_HTTP_REQUEST_TIMEOUT", 60)

	transportConfig := ibclient.NewTransportConfig(
		strconv.FormatBool(infobloxConfig.SSLVerify),
		httpRequestTimeout,
		httpPoolConnections,
	)

	var requestBuilder ibclient.HttpRequestBuilder
	if infobloxConfig.MaxResults != 0 || infobloxConfig.FQDNRexEx != "" {
		// use our own HttpRequestBuilder which sets _max_results parameter on GET requests
		requestBuilder = NewExtendedRequestBuilder(infobloxConfig.MaxResults, infobloxConfig.FQDNRexEx)
	} else {
		// use the default HttpRequestBuilder of the infoblox client
		requestBuilder = &ibclient.WapiRequestBuilder{}
	}

	requestor := &ibclient.WapiHttpRequestor{}

	client, err := ibclient.NewConnector(hostConfig, transportConfig, requestBuilder, requestor)

	if err != nil {
		return nil, err
	}

	provider := &InfobloxProvider{
		client:        client,
		domainFilter:  infobloxConfig.DomainFilter,
		zoneIDFilter:  infobloxConfig.ZoneIDFilter,
		dryRun:        infobloxConfig.DryRun,
		view:          infobloxConfig.View,
		fqdnRegEx:     infobloxConfig.FQDNRexEx,
		createPTR:     infobloxConfig.CreatePTR,
		cacheDuration: infobloxConfig.CacheDuration,
	}

	return provider, nil
}

// Records gets the current records.
func (p *InfobloxProvider) Records(ctx context.Context) (endpoints []*endpoint.Endpoint, err error) {
	zones, err := p.zones()
	if err != nil {
		return nil, fmt.Errorf("could not fetch zones: %s", err)
	}

	for _, zone := range zones {
		logrus.Debugf("fetch records from zone '%s'", zone.Fqdn)
		var resA []ibclient.RecordA
		objA := ibclient.NewRecordA(
			ibclient.RecordA{
				Zone: zone.Fqdn,
				View: p.view,
			},
		)
		err = p.client.GetObject(objA, "", &resA)
		if err != nil {
			return nil, fmt.Errorf("could not fetch A records from zone '%s': %s", zone.Fqdn, err)
		}
		for _, res := range resA {
			newEndpoint := endpoint.NewEndpoint(res.Name, endpoint.RecordTypeA, res.Ipv4Addr)
			if p.createPTR {
				newEndpoint.WithProviderSpecific(providerSpecificInfobloxPtrRecord, "false")
			}
			// Check if endpoint already exists and add to existing endpoint if it does
			foundExisting := false
			for _, ep := range endpoints {
				if ep.DNSName == newEndpoint.DNSName && ep.RecordType == newEndpoint.RecordType {
					logrus.Debugf("Adding target '%s' to existing A record '%s'", newEndpoint.Targets[0], ep.DNSName)
					ep.Targets = append(ep.Targets, newEndpoint.Targets[0])
					foundExisting = true
					break
				}
			}
			if !foundExisting {
				endpoints = append(endpoints, newEndpoint)
			}
		}
		// sort targets so that they are always in same order, as infoblox might return them in different order
		for _, ep := range endpoints {
			sort.Sort(ep.Targets)
		}

		// Include Host records since they should be treated synonymously with A records
		var resH []ibclient.HostRecord
		objH := ibclient.NewHostRecord(
			ibclient.HostRecord{
				Zone: zone.Fqdn,
				View: p.view,
			},
		)
		err = p.client.GetObject(objH, "", &resH)
		if err != nil {
			return nil, fmt.Errorf("could not fetch host records from zone '%s': %s", zone.Fqdn, err)
		}
		for _, res := range resH {
			for _, ip := range res.Ipv4Addrs {
				logrus.Debugf("Record='%s' A(H):'%s'", res.Name, ip.Ipv4Addr)

				// host record is an abstraction in infoblox that combines A and PTR records
				// for any host record we already should have a PTR record in infoblox, so mark it as created
				newEndpoint := endpoint.NewEndpoint(res.Name, endpoint.RecordTypeA, ip.Ipv4Addr)
				if p.createPTR {
					newEndpoint.WithProviderSpecific(providerSpecificInfobloxPtrRecord, "true")
				}
				endpoints = append(endpoints, newEndpoint)
			}
		}

		var resC []ibclient.RecordCNAME
		objC := ibclient.NewRecordCNAME(
			ibclient.RecordCNAME{
				Zone: zone.Fqdn,
				View: p.view,
			},
		)
		err = p.client.GetObject(objC, "", &resC)
		if err != nil {
			return nil, fmt.Errorf("could not fetch CNAME records from zone '%s': %s", zone.Fqdn, err)
		}
		for _, res := range resC {
			logrus.Debugf("Record='%s' CNAME:'%s'", res.Name, res.Canonical)
			endpoints = append(endpoints, endpoint.NewEndpoint(res.Name, endpoint.RecordTypeCNAME, res.Canonical))
		}

		if p.createPTR {
			// infoblox doesn't accept reverse zone's fqdn, and instead expects .in-addr.arpa zone
			// so convert our zone fqdn (if it is a correct cidr block) into in-addr.arpa address and pass that into infoblox
			// example: 10.196.38.0/24 becomes 38.196.10.in-addr.arpa
			arpaZone, err := transform.ReverseDomainName(zone.Fqdn)
			if err == nil {
				var resP []ibclient.RecordPTR
				objP := ibclient.NewRecordPTR(
					ibclient.RecordPTR{
						Zone: arpaZone,
						View: p.view,
					},
				)
				err = p.client.GetObject(objP, "", &resP)
				if err != nil {
					return nil, fmt.Errorf("could not fetch PTR records from zone '%s': %s", zone.Fqdn, err)
				}
				for _, res := range resP {
					endpoints = append(endpoints, endpoint.NewEndpoint(res.PtrdName, endpoint.RecordTypePTR, res.Ipv4Addr))
				}
			}
		}

		var resT []ibclient.RecordTXT
		objT := ibclient.NewRecordTXT(
			ibclient.RecordTXT{
				Zone: zone.Fqdn,
				View: p.view,
			},
		)
		err = p.client.GetObject(objT, "", &resT)
		if err != nil {
			return nil, fmt.Errorf("could not fetch TXT records from zone '%s': %s", zone.Fqdn, err)
		}
		for _, res := range resT {
			// The Infoblox API strips enclosing double quotes from TXT records lacking whitespace.
			// Unhandled, the missing double quotes would break the extractOwnerID method of the registry package.
			if _, err := strconv.Unquote(res.Text); err != nil {
				res.Text = strconv.Quote(res.Text)
			}

			logrus.Debugf("Record='%s' TXT:'%s'", res.Name, res.Text)
			endpoints = append(endpoints, endpoint.NewEndpoint(res.Name, endpoint.RecordTypeTXT, res.Text))
		}
	}

	// update A records that have PTR record created for them already
	if p.createPTR {
		// save all ptr records into map for a quick look up
		ptrRecordsMap := make(map[string]bool)
		for _, ptrRecord := range endpoints {
			if ptrRecord.RecordType != endpoint.RecordTypePTR {
				continue
			}
			ptrRecordsMap[ptrRecord.DNSName] = true
		}

		for i := range endpoints {
			if endpoints[i].RecordType != endpoint.RecordTypeA {
				continue
			}
			// if PTR record already exists for A record, then mark it as such
			if ptrRecordsMap[endpoints[i].DNSName] {
				found := false
				for j := range endpoints[i].ProviderSpecific {
					if endpoints[i].ProviderSpecific[j].Name == providerSpecificInfobloxPtrRecord {
						endpoints[i].ProviderSpecific[j].Value = "true"
						found = true
					}
				}
				if !found {
					endpoints[i].WithProviderSpecific(providerSpecificInfobloxPtrRecord, "true")
				}
			}
		}
	}
	logrus.Debugf("fetched %d records from infoblox", len(endpoints))
	return endpoints, nil
}

func (p *InfobloxProvider) AdjustEndpoints(endpoints []*endpoint.Endpoint) []*endpoint.Endpoint {
	// Update user specified TTL (0 == disabled)
	for i := range endpoints {
		endpoints[i].RecordTTL = endpoint.TTL(p.cacheDuration)
	}

	if !p.createPTR {
		return endpoints
	}

	// for all A records, we want to create PTR records
	// so add provider specific property to track if the record was created or not
	for i := range endpoints {
		if endpoints[i].RecordType == endpoint.RecordTypeA {
			found := false
			for j := range endpoints[i].ProviderSpecific {
				if endpoints[i].ProviderSpecific[j].Name == providerSpecificInfobloxPtrRecord {
					endpoints[i].ProviderSpecific[j].Value = "true"
					found = true
				}
			}
			if !found {
				endpoints[i].WithProviderSpecific(providerSpecificInfobloxPtrRecord, "true")
			}
		}
	}

	return endpoints
}

// ApplyChanges applies the given changes.
func (p *InfobloxProvider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	zones, err := p.zones()
	if err != nil {
		return err
	}

	created, deleted := p.mapChanges(zones, changes)
	p.deleteRecords(deleted)
	p.createRecords(created)
	return nil
}

func (p *InfobloxProvider) zones() ([]ibclient.ZoneAuth, error) {
	var res, result []ibclient.ZoneAuth
	obj := ibclient.NewZoneAuth(
		ibclient.ZoneAuth{
			View: p.view,
		},
	)
	err := p.client.GetObject(obj, "", &res)

	if err != nil {
		return nil, err
	}

	for _, zone := range res {
		if !p.domainFilter.Match(zone.Fqdn) {
			continue
		}

		if !p.zoneIDFilter.Match(zone.Ref) {
			continue
		}

		result = append(result, zone)
	}

	return result, nil
}

type infobloxChangeMap map[string][]*endpoint.Endpoint

func (p *InfobloxProvider) mapChanges(zones []ibclient.ZoneAuth, changes *plan.Changes) (infobloxChangeMap, infobloxChangeMap) {
	created := infobloxChangeMap{}
	deleted := infobloxChangeMap{}

	mapChange := func(changeMap infobloxChangeMap, change *endpoint.Endpoint) {
		zone := p.findZone(zones, change.DNSName)
		if zone == nil {
			logrus.Debugf("Ignoring changes to '%s' because a suitable Infoblox DNS zone was not found.", change.DNSName)
			return
		}
		// Ensure the record type is suitable
		changeMap[zone.Fqdn] = append(changeMap[zone.Fqdn], change)

		if p.createPTR && change.RecordType == endpoint.RecordTypeA {
			reverseZone := p.findReverseZone(zones, change.Targets[0])
			if reverseZone == nil {
				logrus.Debugf("Ignoring changes to '%s' because a suitable Infoblox DNS reverse zone was not found.", change.Targets[0])
				return
			}
			changecopy := *change
			changecopy.RecordType = endpoint.RecordTypePTR
			changeMap[reverseZone.Fqdn] = append(changeMap[reverseZone.Fqdn], &changecopy)
		}
	}

	for _, change := range changes.Delete {
		mapChange(deleted, change)
	}
	for _, change := range changes.UpdateOld {
		mapChange(deleted, change)
	}
	for _, change := range changes.Create {
		mapChange(created, change)
	}
	for _, change := range changes.UpdateNew {
		mapChange(created, change)
	}

	return created, deleted
}

func (p *InfobloxProvider) findZone(zones []ibclient.ZoneAuth, name string) *ibclient.ZoneAuth {
	var result *ibclient.ZoneAuth

	// Go through every zone looking for the longest name (i.e. most specific) as a matching suffix
	for idx := range zones {
		zone := &zones[idx]
		if strings.HasSuffix(name, "."+zone.Fqdn) {
			if result == nil || len(zone.Fqdn) > len(result.Fqdn) {
				result = zone
			}
		} else if strings.EqualFold(name, zone.Fqdn) {
			if result == nil || len(zone.Fqdn) > len(result.Fqdn) {
				result = zone
			}
		}
	}
	return result
}

func (p *InfobloxProvider) findReverseZone(zones []ibclient.ZoneAuth, name string) *ibclient.ZoneAuth {
	ip := net.ParseIP(name)
	networks := map[int]*ibclient.ZoneAuth{}
	maxMask := 0

	for i, zone := range zones {
		_, net, err := net.ParseCIDR(zone.Fqdn)
		if err != nil {
			logrus.WithError(err).Debugf("fqdn %s is no cidr", zone.Fqdn)
		} else {
			if net.Contains(ip) {
				_, mask := net.Mask.Size()
				networks[mask] = &zones[i]
				if mask > maxMask {
					maxMask = mask
				}
			}
		}
	}
	return networks[maxMask]
}

func (p *InfobloxProvider) recordSet(ep *endpoint.Endpoint, getObject bool, targetIndex int) (recordSet infobloxRecordSet, err error) {
	switch ep.RecordType {
	case endpoint.RecordTypeA:
		var res []ibclient.RecordA
		obj := ibclient.NewRecordA(
			ibclient.RecordA{
				Name:     ep.DNSName,
				Ipv4Addr: ep.Targets[targetIndex],
				View:     p.view,
			},
		)
		if getObject {
			err = p.client.GetObject(obj, "", &res)
			if err != nil {
				return
			}
		}
		recordSet = infobloxRecordSet{
			obj: obj,
			res: &res,
		}
	case endpoint.RecordTypePTR:
		var res []ibclient.RecordPTR
		obj := ibclient.NewRecordPTR(
			ibclient.RecordPTR{
				PtrdName: ep.DNSName,
				Ipv4Addr: ep.Targets[targetIndex],
				View:     p.view,
			},
		)
		if getObject {
			err = p.client.GetObject(obj, "", &res)
			if err != nil {
				return
			}
		}
		recordSet = infobloxRecordSet{
			obj: obj,
			res: &res,
		}
	case endpoint.RecordTypeCNAME:
		var res []ibclient.RecordCNAME
		obj := ibclient.NewRecordCNAME(
			ibclient.RecordCNAME{
				Name:      ep.DNSName,
				Canonical: ep.Targets[0],
				View:      p.view,
			},
		)
		if getObject {
			err = p.client.GetObject(obj, "", &res)
			if err != nil {
				return
			}
		}
		recordSet = infobloxRecordSet{
			obj: obj,
			res: &res,
		}
	case endpoint.RecordTypeTXT:
		var res []ibclient.RecordTXT
		// The Infoblox API strips enclosing double quotes from TXT records lacking whitespace.
		// Here we reconcile that fact by making this state match that reality.
		if target, err2 := strconv.Unquote(ep.Targets[0]); err2 == nil && !strings.Contains(ep.Targets[0], " ") {
			ep.Targets = endpoint.Targets{target}
		}
		obj := ibclient.NewRecordTXT(
			ibclient.RecordTXT{
				Name: ep.DNSName,
				Text: ep.Targets[0],
				View: p.view,
			},
		)
		if getObject {
			err = p.client.GetObject(obj, "", &res)
			if err != nil {
				return
			}
		}
		recordSet = infobloxRecordSet{
			obj: obj,
			res: &res,
		}
	}
	return
}

func (p *InfobloxProvider) createRecords(created infobloxChangeMap) {
	for zone, endpoints := range created {
		for _, ep := range endpoints {
			for targetIndex := range ep.Targets {
				if p.dryRun {
					logrus.Infof(

						"Would create %s record named '%s' to '%s' for Infoblox DNS zone '%s'.",
						ep.RecordType,
						ep.DNSName,
						ep.Targets[targetIndex],
						zone,
					)
					continue
				}

				logrus.Infof(
					"Creating %s record named '%s' to '%s' for Infoblox DNS zone '%s'.",
					ep.RecordType,
					ep.DNSName,
					ep.Targets[targetIndex],
					zone,
				)

				recordSet, err := p.recordSet(ep, false, targetIndex)
				if err != nil {
					logrus.Errorf(
						"Failed to retrieve %s record named '%s' to '%s' for DNS zone '%s': %v",
						ep.RecordType,
						ep.DNSName,
						ep.Targets[targetIndex],
						zone,
						err,
					)
					continue
				}
				_, err = p.client.CreateObject(recordSet.obj)
				if err != nil {
					logrus.Errorf(
						"Failed to create %s record named '%s' to '%s' for DNS zone '%s': %v",
						ep.RecordType,
						ep.DNSName,
						ep.Targets[targetIndex],
						zone,
						err,
					)
				}
			}
		}
	}
}

func (p *InfobloxProvider) deleteRecords(deleted infobloxChangeMap) {
	// Delete records first
	for zone, endpoints := range deleted {
		for _, ep := range endpoints {
			for targetIndex := range ep.Targets {
				recordSet, err := p.recordSet(ep, true, targetIndex)
				if err != nil {
					logrus.Errorf(
						"Failed to retrieve %s record named '%s' to '%s' for DNS zone '%s': %v",
						ep.RecordType,
						ep.DNSName,
						ep.Targets[targetIndex],
						zone,
						err,
					)
					continue
				}
				switch ep.RecordType {
				case endpoint.RecordTypeA:
					for _, record := range *recordSet.res.(*[]ibclient.RecordA) {
						if p.dryRun {
							logrus.Infof("Would delete %s record named '%s' to '%s' for Infoblox DNS zone '%s'.", "A", record.Name, record.Ipv4Addr, record.Zone)
						} else {
							logrus.Debugf("Deleting %s record named '%s' to '%s' for Infoblox DNS zone '%s'.", "A", record.Name, record.Ipv4Addr, record.Zone)
							_, err = p.client.DeleteObject(record.Ref)
						}
					}
				case endpoint.RecordTypePTR:
					for _, record := range *recordSet.res.(*[]ibclient.RecordPTR) {
						if p.dryRun {
							logrus.Infof("Would delete %s record named '%s' to '%s' for Infoblox DNS zone '%s'.", "PTR", record.PtrdName, record.Ipv4Addr, record.Zone)
						} else {
							logrus.Debugf("Deleting %s record named '%s' to '%s' for Infoblox DNS zone '%s'.", "PTR", record.PtrdName, record.Ipv4Addr, record.Zone)
							_, err = p.client.DeleteObject(record.Ref)
						}
					}
				case endpoint.RecordTypeCNAME:
					for _, record := range *recordSet.res.(*[]ibclient.RecordCNAME) {
						if p.dryRun {
							logrus.Infof("Would delete %s record named '%s' to '%s' for Infoblox DNS zone '%s'.", "CNAME", record.Name, record.Canonical, record.Zone)
						} else {
							logrus.Debugf("Deleting %s record named '%s' to '%s' for Infoblox DNS zone '%s'.", "CNAME", record.Name, record.Canonical, record.Zone)
							_, err = p.client.DeleteObject(record.Ref)
						}
					}
				case endpoint.RecordTypeTXT:
					for _, record := range *recordSet.res.(*[]ibclient.RecordTXT) {
						if p.dryRun {
							logrus.Infof("Would delete %s record named '%s' to '%s' for Infoblox DNS zone '%s'.", "TXT", record.Name, record.Text, record.Zone)
						} else {
							logrus.Debugf("Deleting %s record named '%s' to '%s' for Infoblox DNS zone '%s'.", "TXT", record.Name, record.Text, record.Zone)
							_, err = p.client.DeleteObject(record.Ref)
						}
					}
				}
				if err != nil {
					logrus.Errorf(
						"Failed to delete %s record named '%s' to '%s' for Infoblox DNS zone '%s': %v",
						ep.RecordType,
						ep.DNSName,
						ep.Targets[targetIndex],
						zone,
						err,
					)
				}
			}
		}
	}
}

func lookupEnvAtoi(key string, fallback int) (i int) {
	val, ok := os.LookupEnv(key)
	if !ok {
		i = fallback
		return
	}
	i, err := strconv.Atoi(val)
	if err != nil {
		i = fallback
		return
	}
	return
}
