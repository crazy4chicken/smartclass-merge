package ffmpeg

import "sort"

// Codec is one codec of the recorder's vocabulary that this service can merge.
// vp8 and vp9 are part of the vocabulary but have no raw elementary-stream
// demuxer in ffmpeg, so they are not representable here (contract §1).
type Codec string

// Supported codecs.
const (
	CodecH264  Codec = "h264"
	CodecH265  Codec = "h265"
	CodecMJPEG Codec = "mjpeg"
	CodecMPEG4 Codec = "mpeg4"
	CodecAV1   Codec = "av1"
)

// Format is the ffmpeg shape of one codec: the demuxer the raw frames are read
// with, the output container and the file extension of the merged object.
type Format struct {
	Demuxer   string
	Container string
	Extension string
}

// formats maps every supported codec onto its copy format (contract §5.4).
var formats = map[Codec]Format{
	CodecH264:  {Demuxer: "h264", Container: "mp4", Extension: ".mp4"},
	CodecH265:  {Demuxer: "hevc", Container: "mp4", Extension: ".mp4"},
	CodecMJPEG: {Demuxer: "mjpeg", Container: "mov", Extension: ".mov"},
	CodecMPEG4: {Demuxer: "m4v", Container: "mp4", Extension: ".mp4"},
	CodecAV1:   {Demuxer: "av1", Container: "mp4", Extension: ".mp4"},
}

// FormatFor returns the ffmpeg shape for codec, and false when codec is not one
// this service merges. With transcode, the video is re-encoded rather than
// copied: the demuxer still matches the recorded codec, while the container and
// extension become mp4/.mp4 because the output is always H.264.
func FormatFor(codec Codec, transcode bool) (Format, bool) {
	format, ok := formats[codec]
	if !ok {
		return Format{}, false
	}
	if transcode {
		format.Container = "mp4"
		format.Extension = ".mp4"
	}
	return format, true
}

// SupportedCodecs lists the codecs this service merges, in alphabetical order.
func SupportedCodecs() []Codec {
	codecs := make([]Codec, 0, len(formats))
	for codec := range formats {
		codecs = append(codecs, codec)
	}
	sort.Slice(codecs, func(i, j int) bool { return codecs[i] < codecs[j] })
	return codecs
}
