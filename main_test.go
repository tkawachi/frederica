package main

import "testing"

func TestGenerateTraceID(t *testing.T) {
	if len(generateTraceID()) != 6 {
		t.Error("TraceID is not 6 characters long")
	}
}
