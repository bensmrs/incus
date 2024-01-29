// Code generated by "libovsdb.modelgen"
// DO NOT EDIT.

package ovsmodel

const LoadBalancerHealthCheckTable = "Load_Balancer_Health_Check"

// LoadBalancerHealthCheck defines an object in Load_Balancer_Health_Check table
type LoadBalancerHealthCheck struct {
	UUID        string            `ovsdb:"_uuid"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	Options     map[string]string `ovsdb:"options"`
	Vip         string            `ovsdb:"vip"`
}