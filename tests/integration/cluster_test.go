// Copyright 2015 The etcd Authors
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

package integration

import (
	"log"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/server/v3/etcdserver"
)

func init() {
	// open microsecond-level time log for integration test debugging
	log.SetFlags(log.Ltime | log.Lmicroseconds | log.Lshortfile)
	if t := os.Getenv("ETCD_ELECTION_TIMEOUT_TICKS"); t != "" {
		if i, err := strconv.ParseInt(t, 10, 64); err == nil {
			electionTicks = int(i)
		}
	}
}

// TestRejectUnhealthyAdd ensures an unhealthy cluster rejects adding members.
func TestRejectUnhealthyAdd(t *testing.T) {
	BeforeTest(t)
	c := newCluster(t, &ClusterConfig{Size: 3, UseBridge: true})
	for _, m := range c.Members {
		m.ServerConfig.StrictReconfigCheck = true
	}
	c.Launch(t)
	defer c.Terminate(t)

	// make cluster unhealthy and wait for downed peer
	c.Members[0].Stop(t)
	c.WaitLeader(t)

	// all attempts to add member should fail
	for i := 1; i < len(c.Members); i++ {
		err := c.addMemberByURL(t, c.URL(i), "unix://foo:12345")
		if err == nil {
			t.Fatalf("should have failed adding peer")
		}
		// TODO: client should return descriptive error codes for internal errors
		if !strings.Contains(err.Error(), "has no leader") {
			t.Errorf("unexpected error (%v)", err)
		}
	}

	// make cluster healthy
	c.Members[0].Restart(t)
	c.WaitLeader(t)
	time.Sleep(2 * etcdserver.HealthInterval)

	// add member should succeed now that it's healthy
	var err error
	for i := 1; i < len(c.Members); i++ {
		if err = c.addMemberByURL(t, c.URL(i), "unix://foo:12345"); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("should have added peer to healthy cluster (%v)", err)
	}
}

// TestRejectUnhealthyRemove ensures an unhealthy cluster rejects removing members
// if quorum will be lost.
func TestRejectUnhealthyRemove(t *testing.T) {
	BeforeTest(t)
	c := newCluster(t, &ClusterConfig{Size: 5, UseBridge: true})
	for _, m := range c.Members {
		m.ServerConfig.StrictReconfigCheck = true
	}
	c.Launch(t)
	defer c.Terminate(t)

	// make cluster unhealthy and wait for downed peer; (3 up, 2 down)
	c.Members[0].Stop(t)
	c.Members[1].Stop(t)
	c.WaitLeader(t)

	// reject remove active member since (3,2)-(1,0) => (2,2) lacks quorum
	err := c.removeMember(t, uint64(c.Members[2].s.ID()))
	if err == nil {
		t.Fatalf("should reject quorum breaking remove")
	}
	// TODO: client should return more descriptive error codes for internal errors
	if !strings.Contains(err.Error(), "has no leader") {
		t.Errorf("unexpected error (%v)", err)
	}

	// member stopped after launch; wait for missing heartbeats
	time.Sleep(time.Duration(electionTicks * int(tickDuration)))

	// permit remove dead member since (3,2) - (0,1) => (3,1) has quorum
	if err = c.removeMember(t, uint64(c.Members[0].s.ID())); err != nil {
		t.Fatalf("should accept removing down member")
	}

	// bring cluster to (4,1)
	c.Members[0].Restart(t)

	// restarted member must be connected for a HealthInterval before remove is accepted
	time.Sleep((3 * etcdserver.HealthInterval) / 2)

	// accept remove member since (4,1)-(1,0) => (3,1) has quorum
	if err = c.removeMember(t, uint64(c.Members[0].s.ID())); err != nil {
		t.Fatalf("expected to remove member, got error %v", err)
	}
}

// TestRestartRemoved ensures that restarting removed member must exit
// if 'initial-cluster-state' is set 'new' and old data directory still exists
// (see https://github.com/etcd-io/etcd/issues/7512 for more).
func TestRestartRemoved(t *testing.T) {
	BeforeTest(t)

	// 1. start single-member cluster
	c := newCluster(t, &ClusterConfig{Size: 1, UseBridge: true})
	for _, m := range c.Members {
		m.ServerConfig.StrictReconfigCheck = true
	}
	c.Launch(t)
	defer c.Terminate(t)

	// 2. add a new member
	c.AddMember(t)
	c.WaitLeader(t)

	oldm := c.Members[0]
	oldm.keepDataDirTerminate = true

	// 3. remove first member, shut down without deleting data
	if err := c.removeMember(t, uint64(c.Members[0].s.ID())); err != nil {
		t.Fatalf("expected to remove member, got error %v", err)
	}
	c.WaitLeader(t)

	// 4. restart first member with 'initial-cluster-state=new'
	// wrong config, expects exit within ReqTimeout
	oldm.ServerConfig.NewCluster = false
	if err := oldm.Restart(t); err != nil {
		t.Fatalf("unexpected ForceRestart error: %v", err)
	}
	defer func() {
		oldm.Close()
		os.RemoveAll(oldm.ServerConfig.DataDir)
	}()
	select {
	case <-oldm.s.StopNotify():
	case <-time.After(time.Minute):
		t.Fatalf("removed member didn't exit within %v", time.Minute)
	}
}

func TestSpeedyTerminate(t *testing.T) {
	BeforeTest(t)
	clus := NewClusterV3(t, &ClusterConfig{Size: 3, UseBridge: true})
	// Stop/Restart so requests will time out on lost leaders
	for i := 0; i < 3; i++ {
		clus.Members[i].Stop(t)
		clus.Members[i].Restart(t)
	}
	donec := make(chan struct{})
	go func() {
		defer close(donec)
		clus.Terminate(t)
	}()
	select {
	case <-time.After(10 * time.Second):
		t.Fatalf("cluster took too long to terminate")
	case <-donec:
	}
}
