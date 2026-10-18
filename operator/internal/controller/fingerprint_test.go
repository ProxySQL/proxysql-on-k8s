package controller

import (
	"testing"

	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
)

func TestSyncFingerprint_ChangesWithSQLStatements(t *testing.T) {
	addrs := []string{"10.0.0.1:6032"}
	base := syncFingerprint(&proxysqlclient.Desired{}, addrs)
	withStmt := syncFingerprint(&proxysqlclient.Desired{
		SQLStatements: []string{"PROXYSQL FLUSH QUERY CACHE"},
	}, addrs)
	if base == withStmt {
		t.Fatal("adding sqlStatements must change the sync fingerprint")
	}
	edited := syncFingerprint(&proxysqlclient.Desired{
		SQLStatements: []string{"PROXYSQL FLUSH MYSQL QUERY CACHE"},
	}, addrs)
	if withStmt == edited {
		t.Fatal("editing a statement must change the sync fingerprint")
	}
}

// upgradeStableDesired is a fixed spec that never sets MySQLServer.Status.
func upgradeStableDesired() *proxysqlclient.Desired {
	w := int32(10)
	return &proxysqlclient.Desired{
		MySQLServers: []proxysqlclient.MySQLServer{
			{Hostgroup: 10, Hostname: "db-0", Port: 3306, Weight: &w, Comment: "primary"},
			{Hostgroup: 20, Hostname: "db-1", Port: 3306},
		},
	}
}

// Adding MySQLServer.Status must not change the fingerprint of a spec that
// does not set it: a changed fingerprint forces a one-time full re-push to
// every cluster on operator upgrade. The pinned value was computed with the
// operator at v0.7.0 (b3c2038), before the field existed.
func TestSyncFingerprint_UnsetServerStatusIsUpgradeStable(t *testing.T) {
	const atV070 = "cbe50a9af6bcfb6df38f307fa7205462134c3d0294f27499b2407ed97c8fc6b1"
	if got := syncFingerprint(upgradeStableDesired(), []string{"10.0.0.1:6032"}); got != atV070 {
		t.Fatalf("fingerprint for a status-less spec changed: got %s, want %s", got, atV070)
	}
}

// Setting (or changing) a server's status is a spec change: it must bust the
// hash short-circuit so the new status is pushed.
func TestSyncFingerprint_ChangesWithServerStatus(t *testing.T) {
	addrs := []string{"10.0.0.1:6032"}
	base := syncFingerprint(upgradeStableDesired(), addrs)
	soft := upgradeStableDesired()
	soft.MySQLServers[1].Status = "OFFLINE_SOFT"
	hard := upgradeStableDesired()
	hard.MySQLServers[1].Status = "OFFLINE_HARD"
	fs, fh := syncFingerprint(soft, addrs), syncFingerprint(hard, addrs)
	if base == fs || base == fh || fs == fh {
		t.Fatalf("status must change the fingerprint: base=%s soft=%s hard=%s", base, fs, fh)
	}
}
