//go:build realdb

package main

import "testing"

func TestRealRequestReadSnapshot(t *testing.T)     { runReadSnapshotContract(t, realContractFactory) }
func TestRealProactiveCompactionEval(t *testing.T) { runProactiveEval(t, realContractFactory) }
