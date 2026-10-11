//go:build linux && amd64

package ffmpeg

import "embed"

// platformBinaryName is the file name of the embedded build for this platform;
// it is also the name the payload is extracted under.
const platformBinaryName = "ffmpeg-linux-amd64"

// binDir carries the ffmpeg payloads present at build time: the directory, not
// one fixed file, so a checkout whose payload was not fetched yet still compiles
// and reports the missing build at startup instead of failing `go build`
// (contract §6). Fetch only the platform you build for — everything in bin/ is
// embedded into the binary.
//
//go:embed all:bin
var binDir embed.FS
