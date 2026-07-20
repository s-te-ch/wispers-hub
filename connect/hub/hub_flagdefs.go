// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// Custom flag definitions.
package main

import "fmt"

type RunMode string

const (
	Standalone RunMode = "standalone"
	Managed    RunMode = "managed"
)

func (l *RunMode) String() string {
	return string(*l)
}

func (l *RunMode) Set(value string) error {
	switch RunMode(value) {
	case Standalone, Managed:
		*l = RunMode(value)
		return nil
	}
	return fmt.Errorf("must be one of: standalone, managed")
}
