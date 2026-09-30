//go:build !windows

package config

func localDriveRoots() []string { return nil }

func oneDriveLibraries() []CloudFolder { return nil }
