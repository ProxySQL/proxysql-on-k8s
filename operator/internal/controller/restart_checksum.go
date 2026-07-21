/*
Copyright 2026 ProxySQL.

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

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
)

const (
	// annotationCnfChecksum is the pod-template annotation that drives a
	// rolling restart when it changes (builders.Builder.StatefulSet).
	annotationCnfChecksum = "proxysql.com/cnf-checksum"

	// annotationVarsAppliedHash is an OBJECT-level (never pod-template) STS
	// annotation: the SHA-256 of the sorted variable set that was last
	// successfully applied, whether via runtime SQL or a restart. It closes
	// the crash-safety window between the cnf Secret update (spec step 1)
	// and the runtime SQL push (spec step 2) — see resolveRestartChecksum.
	annotationVarsAppliedHash = "proxysql.com/vars-applied-hash"
)

// cnfVerdict classifies a cnf change for resolveRestartChecksum's caller.
type cnfVerdict int

const (
	// verdictBootHash: fresh StatefulSet, already booted on this exact cnf,
	// or no prior Secret to diff against — adopt newHash outright.
	verdictBootHash cnfVerdict = iota
	// verdictKeepPrev: nothing to apply (no variable-level diff and the
	// vars-applied-hash marker already matches) — keep the pod-template
	// annotation exactly as it is so no restart is triggered.
	verdictKeepPrev
	// verdictRuntimeTry: attempt a restart-free runtime apply of `changed`.
	verdictRuntimeTry
	// verdictStructural: the cnf changed outside the runtime-appliable
	// variable set (or a variable was added/removed) — a rolling restart is
	// required.
	verdictStructural
)

// classifyCnfChange is the pure decision core of resolveRestartChecksum: it
// takes no I/O and does not dial ProxySQL, so it's exhaustively unit-tested
// on its own (restart_checksum_test.go). oldData/newData are the FULL cnf
// Secret data maps before/after this reconcile (nil/empty oldData means the
// Secret didn't exist); prev is the pod-template proxysql.com/cnf-checksum
// annotation before this reconcile ("" if no StatefulSet yet); newHash is
// the freshly computed builders.CnfChecksum of newData; appliedVars is the
// current proxysql.com/vars-applied-hash STS object annotation ("" if
// absent).
//
// The runtime-apply relaxation exists ONLY for value-level changes inside
// the "proxysql.cnf" key. Every other Secret key (fluent-bit.conf) is
// consumed by a container at startup, so any difference there — content,
// or the key appearing/disappearing — is structural and must restart;
// structuralKeys names the offending keys in that case.
//
// changed is populated only for verdictRuntimeTry: the full-name variable
// map to push at runtime. It is the diff (oldVars vs newVars) when
// non-empty, or the FULL new variable set when the diff is empty but
// appliedVars is stale — the crash-recovery case where the Secret was
// already updated but the operator died before confirming the runtime push
// (idempotent UPDATEs make re-pushing the full set safe).
func classifyCnfChange(oldData, newData map[string][]byte, prev, newHash, appliedVars string) (verdict cnfVerdict, changed map[string]string, structuralKeys []string) {
	oldCnf := string(oldData["proxysql.cnf"])
	newCnf := string(newData["proxysql.cnf"])
	newVars := builders.ParseCnfVariables(newCnf)

	if prev == "" || prev == newHash {
		return verdictBootHash, nil, nil
	}
	if len(oldData) == 0 || oldCnf == "" {
		return verdictBootHash, nil, nil
	}
	// Any difference outside proxysql.cnf — a key added, removed, or with
	// different content — is structural. Checked BEFORE the proxysql.cnf
	// normalization so a Secret-wide change can never be misread as
	// variables-only just because proxysql.cnf itself didn't move.
	if keys := diffNonCnfKeys(oldData, newData); len(keys) > 0 {
		return verdictStructural, nil, keys
	}
	if builders.NormalizeCnf(oldCnf) != builders.NormalizeCnf(newCnf) {
		return verdictStructural, nil, nil
	}

	oldVars := builders.ParseCnfVariables(oldCnf)
	changedVars := make(map[string]string)
	for k, v := range newVars {
		if oldVars[k] != v {
			changedVars[k] = v
		}
	}

	if len(changedVars) == 0 {
		newVarsHash := varsHash(newVars)
		if appliedVars == newVarsHash {
			return verdictKeepPrev, nil, nil
		}
		// Crash recovery: the Secret already carries newCnf (oldCnf==newCnf)
		// but the marker doesn't match, so the last runtime push either never
		// happened or never got confirmed. Push the full set again.
		return verdictRuntimeTry, newVars, nil
	}

	return verdictRuntimeTry, changedVars, nil
}

// diffNonCnfKeys returns the sorted set of Secret keys OTHER than
// "proxysql.cnf" that differ between the two data maps — missing on either
// side, or present on both with different bytes. Any such key makes a
// change structural: those keys (fluent-bit.conf) are read by containers at
// startup, so only a restart propagates them.
func diffNonCnfKeys(oldData, newData map[string][]byte) []string {
	seen := map[string]struct{}{}
	var keys []string
	check := func(k string) {
		if k == "proxysql.cnf" {
			return
		}
		if _, done := seen[k]; done {
			return
		}
		seen[k] = struct{}{}
		oldV, inOld := oldData[k]
		newV, inNew := newData[k]
		if inOld != inNew || !bytes.Equal(oldV, newV) {
			keys = append(keys, k)
		}
	}
	for k := range oldData {
		check(k)
	}
	for k := range newData {
		check(k)
	}
	sort.Strings(keys)
	return keys
}

// varsHash returns a deterministic SHA-256 hex digest over a variable map:
// sorted "key=value" lines, one per line.
func varsHash(vars map[string]string) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%s=%s\n", k, vars[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// domainForFullName maps a full-name cnf variable ("mysql-max_connections")
// to the ProxySQL admin LOAD/SAVE domain its bare name lives under.
func domainForFullName(fullName string) (domain string, ok bool) {
	switch {
	case strings.HasPrefix(fullName, "admin-"):
		return "ADMIN", true
	case strings.HasPrefix(fullName, "mysql-"):
		return "MYSQL", true
	case strings.HasPrefix(fullName, "pgsql-"):
		return "PGSQL", true
	default:
		return "", false
	}
}

// groupByDomain partitions full-name variables by their LOAD/SAVE domain.
func groupByDomain(vars map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string)
	for k, v := range vars {
		domain, ok := domainForFullName(k)
		if !ok {
			// ParseCnfVariables only ever emits admin-/mysql-/pgsql- keys;
			// this is unreachable in practice but skipped defensively rather
			// than risking a misrouted write.
			continue
		}
		if out[domain] == nil {
			out[domain] = make(map[string]string)
		}
		out[domain][k] = v
	}
	return out
}

// sortedKeys returns the sorted keys of a string-keyed map.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveRestartChecksum decides what proxysql.com/cnf-checksum the pod
// template should carry, and what proxysql.com/vars-applied-hash the
// StatefulSet OBJECT should carry, for this reconcile.
//
// oldData is the full cnf Secret data map BEFORE this reconcile updated it
// (nil if the Secret didn't exist). newData is the full data map this
// reconcile is about to write. prev is the StatefulSet's current
// pod-template proxysql.com/cnf-checksum annotation ("" if no StatefulSet
// exists yet). newHash is builders.CnfChecksum over newData. appliedVars is
// the StatefulSet's current OBJECT-level proxysql.com/vars-applied-hash
// annotation ("" if absent).
//
// Returns the pod-template checksum annotation to write, the object-level
// vars-applied-hash annotation to write, and a human summary for the
// Progressing status condition: "" (no news), "RuntimeApplied: <keys>", or
// "RestartRequired: <reason>". A non-nil error means the runtime SQL push
// failed partway through — the caller must requeue without advancing the
// vars-applied-hash annotation, so the retry re-pushes the same variables.
func (r *ProxySQLClusterReconciler) resolveRestartChecksum(
	ctx context.Context,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	oldData, newData map[string][]byte,
	prev, newHash, appliedVars string,
	radminPassword string,
) (annotation, appliedVarsHash, summary string, err error) {
	newVars := builders.ParseCnfVariables(string(newData["proxysql.cnf"]))
	newVarsHash := varsHash(newVars)

	verdict, changed, structuralKeys := classifyCnfChange(oldData, newData, prev, newHash, appliedVars)

	switch verdict {
	case verdictBootHash:
		return newHash, newVarsHash, "", nil
	case verdictStructural:
		msg := "RestartRequired: structural cnf change"
		if len(structuralKeys) > 0 {
			msg = fmt.Sprintf("RestartRequired: structural cnf change (%s)", strings.Join(structuralKeys, ", "))
		}
		return newHash, newVarsHash, msg, nil
	case verdictKeepPrev:
		return prev, newVarsHash, "", nil
	}

	// verdictRuntimeTry: dial every ready pod and push `changed` at runtime.
	adminPort := builders.DefaultedSpec(cluster).Protocols.Admin.Port
	addrs, derr := discoverPodAddresses(ctx, r.Client, cluster, adminPort)
	if derr != nil {
		return "", "", "", derr
	}
	if len(addrs) == 0 {
		// Nothing running yet; pods will bootstrap straight from the
		// (already-updated) Secret. Keep the annotation as-is — no restart
		// needed once pods do come up, they'll read newCnf from the volume.
		return prev, newVarsHash, "", nil
	}

	byDomain := groupByDomain(changed)
	keys := sortedKeys(changed)
	mismatched := make(map[string]struct{})

	for _, addr := range addrs {
		pxc, cerr := proxysqlclient.New(addr, "radmin", radminPassword)
		if cerr != nil {
			return "", "", "", fmt.Errorf("dial %s: %w", addr, cerr)
		}

		applyErr := func() error {
			defer func() { _ = pxc.Close() }()
			for _, domain := range []string{"ADMIN", "MYSQL", "PGSQL"} {
				vars := byDomain[domain]
				if len(vars) == 0 {
					continue
				}
				if err := proxysqlclient.ApplyVariables(ctx, pxc, vars, domain); err != nil {
					return fmt.Errorf("apply variables on %s: %w", addr, err)
				}
			}
			got, err := proxysqlclient.ReadGlobalVariables(ctx, pxc, keys)
			if err != nil {
				return fmt.Errorf("read back variables on %s: %w", addr, err)
			}
			for k, want := range changed {
				if got[k] != want {
					mismatched[k] = struct{}{}
				}
			}
			return nil
		}()
		if applyErr != nil {
			return "", "", "", applyErr
		}
	}

	if len(mismatched) > 0 {
		names := make([]string, 0, len(mismatched))
		for k := range mismatched {
			names = append(names, k)
		}
		sort.Strings(names)
		return newHash, newVarsHash,
			fmt.Sprintf("RestartRequired: %s (runtime read-back mismatch)", strings.Join(names, ", ")), nil
	}

	return prev, newVarsHash, fmt.Sprintf("RuntimeApplied: %s", strings.Join(keys, ", ")), nil
}
