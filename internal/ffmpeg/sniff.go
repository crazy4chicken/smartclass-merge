package ffmpeg

// codec sniffing constants: the NAL header bytes that identify a codec without
// ambiguity. Every other header byte is readable as both an H.264 NAL type and an
// H.265 NAL type, which is why only these shapes decide (see Sniff).
const (
	// h264SPSNal and friends are H.264 parameter sets with nal_ref_idc 1 or 3:
	// 0x67/0x68 are the common SPS/PPS, 0x27/0x28 the same types with
	// nal_ref_idc 1. Read as H.265 they are NAL types 51..52, which no encoder
	// emits, so they prove H.264.
	h264SPSNal = 0x67
	h264SPSAlt = 0x27
	h264PPSNal = 0x68
	h264PPSAlt = 0x28
	// hevcVPSNal, hevcSPSNal and hevcPPSNal are H.265 NAL types 32..34. Read as
	// H.264 they are NAL types 0 (invalid), 2 and 4 (slice data partitions,
	// which encoders do not emit), so they prove H.265.
	hevcVPSNal = 0x40
	hevcSPSNal = 0x42
	hevcPPSNal = 0x44
	// mpeg4VOPNal is the MPEG-4 visual object plane start code payload. Read as
	// an H.264 NAL it is type 22 (reserved), which encoders do not emit.
	mpeg4VOPNal = 0xB6
)

// Sniff classifies the encoded frames of a recording from their leading bytes.
// The caller passes the concatenation of the first frames of the recording's
// first segment (the merge engine reads a bounded window), because a single
// frame usually holds only a slice while the parameter sets that identify the
// codec arrive with the first keyframe.
//
// The recorder stores bare codec output with no container and no declared codec,
// so the frames are inspected directly:
//
//   - a JPEG SOI marker is mjpeg;
//   - an AV1 temporal-delimiter OBU is av1;
//   - an H.265 VPS/SPS/PPS NAL header is h265;
//   - an H.264 SPS/PPS NAL header is h264;
//   - an MPEG-4 visual object plane start code is mpeg4.
//
// H.264 and H.265 header bytes overlap almost everywhere: an H.264 slice with
// nal_ref_idc 2 (0x41..0x51) reads as an H.265 NAL of type 32..40, exactly like
// the H.265 parameter sets, and H.265 slice types (0x02, 0x26, ...) read as
// valid H.264 NAL types. Slice traffic alone therefore never identifies a codec,
// and this function reports false rather than guessing: an H.264 stream misread
// as H.265 (or the reverse) fails inside ffmpeg with an error that names neither
// the cause nor the fix. Only parameter sets decide, and the caller should pass
// the codec explicitly whenever it knows it.
//
// vp8 and vp9 have no start code and no fixed signature, so they are never
// reported; the service rejects them because raw vp8/vp9 elementary streams have
// no ffmpeg demuxer.
func Sniff(frame []byte) (Codec, bool) {
	if len(frame) >= 3 && frame[0] == 0xFF && frame[1] == 0xD8 && frame[2] == 0xFF {
		return CodecMJPEG, true
	}
	// An AV1 temporal delimiter OBU: forbidden bit clear, type 2, an extension
	// flag of zero, a size field present and the reserved bit set.
	if len(frame) >= 2 && frame[0] == 0x12 && frame[1] == 0x00 {
		return CodecAV1, true
	}
	var h264, h265, mpeg4 bool
	// A start code is three bytes followed by the NAL header byte, so the last
	// position that can hold one is len(frame)-4.
	for i := range len(frame) - 3 {
		if frame[i] != 0 || frame[i+1] != 0 || frame[i+2] != 1 {
			continue
		}
		header := frame[i+3]
		switch header {
		case h264SPSNal, h264SPSAlt, h264PPSNal, h264PPSAlt:
			h264 = true
		case hevcVPSNal, hevcSPSNal, hevcPPSNal:
			// The NAL header of H.265 is two bytes: the second one carries
			// nuh_layer_id and a temporal_id_plus1 that the format requires to
			// be at least one. A zero there means these bytes are not an H.265
			// header after all.
			if i+4 == len(frame) || frame[i+4]&0x07 != 0 {
				h265 = true
			}
		case mpeg4VOPNal:
			mpeg4 = true
		}
	}
	switch {
	case h265:
		return CodecH265, true
	case h264:
		return CodecH264, true
	case mpeg4:
		return CodecMPEG4, true
	}
	return "", false
}
