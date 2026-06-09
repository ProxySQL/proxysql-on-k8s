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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResyncDue(t *testing.T) {
	now := time.Now()
	interval := 2 * time.Minute
	tm := func(d time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(d)); return &t }

	cases := []struct {
		name string
		last *metav1.Time
		want bool
	}{
		{"never synced -> due", nil, true},
		{"synced just now -> not due", tm(0), false},
		{"synced 1m ago -> not due", tm(-1 * time.Minute), false},
		{"synced exactly interval ago -> due", tm(-2 * time.Minute), true},
		{"synced well past interval -> due", tm(-10 * time.Minute), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resyncDue(c.last, now, interval); got != c.want {
				t.Errorf("resyncDue=%v want %v", got, c.want)
			}
		})
	}
}
