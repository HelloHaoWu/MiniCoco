//go:build realdb

package main

import "testing"

func TestRealReusePersistence(t *testing.T) { runReusePersistence(t, realContractFactory) }

func TestRealReuseRecovery(t *testing.T) { runReuseRecovery(t, realContractFactory) }
