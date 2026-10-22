package main

import (
	"errors"
	"testing"
	"time"
)

func TestStreamPreflightReadyReleases(t *testing.T) {
	p := newStreamPreflight()
	if !p.markReady() {
		t.Fatal("markReady returned false")
	}
	select {
	case <-p.ready:
	case <-time.After(time.Second):
		t.Fatal("ready was not closed")
	}
	p.releaseNow()
	select {
	case <-p.release:
	case <-time.After(time.Second):
		t.Fatal("release was not closed")
	}
	if p.fail(errors.New("late error")) {
		t.Fatal("fail should be rejected after ready")
	}
}

func TestStreamPreflightFailBeforeContent(t *testing.T) {
	p := newStreamPreflight()
	if !p.fail(errors.New("queue")) {
		t.Fatal("fail returned false")
	}
	select {
	case err := <-p.failed:
		if err == nil || err.Error() != "queue" {
			t.Fatalf("failed = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed was not delivered")
	}
	if p.markReady() {
		t.Fatal("markReady should be rejected after fail")
	}
}

func TestStreamPreflightAbandonAllowsLaterErrors(t *testing.T) {
	p := newStreamPreflight()
	p.abandon()
	select {
	case <-p.release:
	case <-time.After(time.Second):
		t.Fatal("abandon did not release")
	}
	if p.markReady() {
		t.Fatal("markReady should be rejected after abandon")
	}
	if p.fail(errors.New("late error")) {
		t.Fatal("fail should be rejected after abandon")
	}
}
