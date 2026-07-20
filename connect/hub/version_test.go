// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package hubsrv

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The floor check under an armed floor: MinClientVersion is 0.0.0 (disarmed)
// for now, but the rejection machinery must stay ready — including the
// message prefix that hub.rs matches on to classify the error.
func TestCheckClientVersion(t *testing.T) {
	defer func() { minClientParsed = mustParseVersion(MinClientVersion) }()

	cases := []struct {
		name      string
		version   string // "" = no wispers-client-version header
		floor     string
		wantCode  codes.Code
		wantInMsg string
	}{
		{"below floor", "0.12.0", "0.13.2", codes.FailedPrecondition, versionRejectionPrefix},
		{"floor in message", "0.12.0", "0.13.2", codes.FailedPrecondition, "v0.13.2"},
		{"client version in message", "0.12.0", "0.13.2", codes.FailedPrecondition, "got v0.12.0"},
		{"at floor", "0.13.2", "0.13.2", codes.OK, ""},
		{"above floor", "1.0.0", "0.13.2", codes.OK, ""},
		{"prerelease counts as its release", "0.13.2-rc1", "0.13.2", codes.OK, ""},
		{"unversioned counts as 0.0.0", "", "0.13.2", codes.FailedPrecondition, "got v0.0.0"},
		{"unversioned vs disarmed floor", "", "0.0.0", codes.OK, ""},
		{"anything vs disarmed floor", "0.0.1", "0.0.0", codes.OK, ""},
		{"malformed names the input", "yolo", "0.0.0", codes.InvalidArgument, `"yolo"`},
	}
	for _, tc := range cases {
		minClientParsed = mustParseVersion(tc.floor)
		md := metadata.MD{}
		if tc.version != "" {
			md = metadata.Pairs(clientVersionKey, tc.version)
		}
		err := checkClientVersion(metadata.NewIncomingContext(context.Background(), md))
		if got := status.Code(err); got != tc.wantCode {
			t.Errorf("%s: got %v (%v), want %v", tc.name, got, err, tc.wantCode)
		}
		if err != nil && !strings.Contains(err.Error(), tc.wantInMsg) {
			t.Errorf("%s: message %q does not contain %q", tc.name, err, tc.wantInMsg)
		}
	}
}
