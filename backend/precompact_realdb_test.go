//go:build realdb

package main

import "testing"

func TestRealPrecompactPersistence(t *testing.T) { runPrecompactPersistence(t, realContractFactory) }
