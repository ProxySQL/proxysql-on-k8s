package rdsconfig

import (
	"testing"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/rdsclient"
)

func TestFromSpecPreservesRDSRequestTargets(t *testing.T) {
	source := &proxysqlv1alpha1.AWSDBProxySpec{
		EngineFamily: "MYSQL",
		TargetGroups: []proxysqlv1alpha1.AWSTargetGroupSpec{{TargetGroupName: "default"}},
		Targets: []proxysqlv1alpha1.AWSTargetSpec{{
			TargetGroupName: "default", DBClusterIdentifiers: []string{"aurora-orders"}, DBInstanceIdentifiers: []string{"orders-1"},
		}},
	}
	desired, err := FromSpec(source, "orders")
	if err != nil {
		t.Fatalf("FromSpec: %v", err)
	}
	if desired.Proxy.Name != "orders" || len(desired.Targets) != 2 {
		t.Fatalf("desired = %+v", desired)
	}
	if desired.Targets[0] != (rdsclient.Target{Identifier: "orders-1", Type: "RDS_INSTANCE"}) ||
		desired.Targets[1] != (rdsclient.Target{Identifier: "aurora-orders", Type: "TRACKED_CLUSTER"}) {
		t.Fatalf("targets = %+v", desired.Targets)
	}
}

func TestDesiredDriftNormalizesOrderAndDefaults(t *testing.T) {
	desired := Desired{
		Proxy: rdsclient.Proxy{Name: "orders", ARN: "arn:proxy", EngineFamily: "MYSQL", DefaultAuthScheme: "NONE", IdleClientTimeout: 1800,
			VpcSecurityGroupIDs: []string{"sg-b", "sg-a"}, Auth: []rdsclient.Auth{}},
		TargetGroup: rdsclient.TargetGroup{Name: "default", Pool: rdsclient.Pool{ConnectionBorrowTimeout: 120, MaxConnectionsPercent: 100, MaxIdleConnectionsPercent: 50}},
		Targets:     []rdsclient.Target{{Identifier: "db-b", Type: "RDS_INSTANCE"}, {Identifier: "db-a", Type: "RDS_INSTANCE"}},
		Tags:        []rdsclient.Tag{{Key: "team", Value: "payments"}},
	}
	observed := rdsclient.State{
		Proxy: rdsclient.Proxy{Name: "orders", ARN: "arn:proxy", EngineFamily: "MYSQL", DefaultAuthScheme: "NONE", IdleClientTimeout: 1800,
			VpcSecurityGroupIDs: []string{"sg-a", "sg-b"}, Auth: []rdsclient.Auth{}},
		TargetGroup: desired.TargetGroup,
		Targets:     []rdsclient.Target{{Identifier: "db-a", Type: "RDS_INSTANCE"}, {Identifier: "db-b", Type: "RDS_INSTANCE"}},
		Tags:        desired.Tags,
	}
	if drift := Drift(desired, observed); len(drift) != 0 {
		t.Fatalf("Drift = %v, want none", drift)
	}
	observed.Targets = nil
	if drift := Drift(desired, observed); len(drift) != 1 || drift[0] != "targets" {
		t.Fatalf("Drift = %v, want targets", drift)
	}
}
