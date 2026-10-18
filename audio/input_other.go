//go:build !darwin && !(linux && (amd64 || arm64)) && !(windows && (amd64 || arm64)) && !js

package audio

func openInputDevice() (inputDevice, error) { return nil, errUnsupportedInput }
