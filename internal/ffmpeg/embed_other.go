//go:build !(windows && amd64) && !(linux && amd64) && !(linux && arm64)

package ffmpeg

import "embed"

// platformBinaryName is empty on platforms without an embedded build, so nothing
// can be looked up or extracted.
const platformBinaryName = ""

// binDir is empty on platforms without an embedded build: this build carries
// ffmpeg payloads for windows/amd64, linux/amd64 and linux/arm64 only (contract
// §6). Path reports errNoEmbedded, and MERGE_FFMPEG_PATH must point at an
// existing ffmpeg executable.
var binDir embed.FS
