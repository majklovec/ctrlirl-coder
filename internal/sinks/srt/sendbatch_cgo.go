// C-layer batched SRT send. The Go Write hot path used to make one cgo
// crossing per 1316-byte SRT packet (srtgo.SrtSocket.Write → srt_sendmsg2).
// At ~6 Mbps that is ~30–40 k crossings/s; each Go↔C round-trip plus srtgo's
// per-packet poll bookkeeping costs enough that the send path itself becomes
// chronic backpressure and the GStreamer appsink starts dropping buffers —
// the source of the recurring content hole that surfaces as the HLS 10 s
// audio/video glitch.
//
// This file collapses the whole send of a multi-packet batch into ONE cgo
// crossing: the C function below splits the batch into SRT live-safe 1316-byte
// chunks and calls srt_sendmsg2 in a C loop, doing the non-blocking
// write-ready wait (srt_epoll) entirely in C. Go round-trips once per batch
// (tens of KB) instead of once per 1316-byte packet, so the send path no longer
// clogs and the downstream backpressure that produced the dip disappears.
//
// It reuses the exact SRTSOCKET srtgo already created (via SrtSocket.GetSocket)
// and links the same libsrt the rest of the process uses, so socket lifecycle
// and statistics stay owned by srtgo — only the hot send path moves to raw cgo.
package srt

/*
#cgo LDFLAGS: -lsrt
#include <srt/srt.h>
#include <time.h>

// Monotonic milliseconds, for the send budget clock.
static long srt_send_now_ms(void) {
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (long)ts.tv_sec * 1000 + (long)(ts.tv_nsec / 1000000);
}

// srt_send_batch pushes up to `total` bytes to `u` in a single cgo crossing.
// It splits the payload into SRT live-safe 1316-byte chunks and sends each with
// srt_sendmsg2. The socket is non-blocking: when a chunk can't be accepted
// (SRT_EASYNCSND / SRT_ETIMEOUT) it waits for write readiness on an SRT epoll
// for up to `poll_ms`, retrying until every byte is sent or the total
// `budget_ms` window is exhausted.
//
// Returns the number of bytes sent (>= 0). On completion *srterror is 0; on a
// failure it is the libsrt SRT error code (temporary = budget exhausted, send
// buffer not ready — link healthy; anything else = hard connection loss).
// *syserror carries the underlying OS errno.
long srt_send_batch(SRTSOCKET u, const char* buf, long total,
					long poll_ms, long budget_ms,
					int* srterror, int* syserror) {
	*srterror = 0;
	*syserror = 0;
	if (total <= 0) {
		return 0;
	}

	int eid = srt_epoll_create();
	if (eid < 0) {
		*srterror = -1;
		*syserror = 1;
		return 0;
	}
	int ev = SRT_EPOLL_OUT;
	srt_epoll_add_usock(eid, u, &ev);

	long sent = 0;
	long start = srt_send_now_ms();
	while (sent < total) {
		int chunk = (int)((total - sent) > 1316 ? 1316 : (total - sent));
		int r = srt_sendmsg2(u, buf + sent, chunk, NULL);
		if (r > 0) {
			sent += r;
			continue;
		}

		int sys = 0;
		int srterr = srt_getlasterror(&sys);

		if (srterr == SRT_EASYNCSND || srterr == SRT_ETIMEOUT) {
			long elapsed = srt_send_now_ms() - start;
			if (elapsed >= budget_ms) {
				*srterror = srterr;
				*syserror = sys;
				break;
			}
			long pms = budget_ms - elapsed;
			if (pms > poll_ms) {
				pms = poll_ms;
			}
			int wnum = 0;
			srt_epoll_wait(eid, NULL, NULL, &u, &wnum, (int64_t)pms,
							NULL, NULL, NULL, NULL);
			continue;
		}

		// Hard error (connection lost, closed, invalid socket, ...).
		*srterror = srterr;
		*syserror = sys;
		break;
	}

	srt_epoll_remove_usock(eid, u);
	srt_epoll_release(eid);
	return sent;
}
*/
import "C"

import (
	"unsafe"

	"github.com/haivision/srtgo"
)

// cgoSendBatch sends the whole batch `b` to `sock` in a single cgo crossing;
// the C side splits it into SRT live-safe chunks and waits on write readiness
// in C. It returns the bytes sent and the libsrt SRT error code (0 = fully
// sent; a temporary code = budget exhausted; anything else = hard loss).
func cgoSendBatch(sock *srtgo.SrtSocket, b []byte) (int, srtgo.SRTErrno) {
	if len(b) == 0 {
		return 0, 0
	}
	u := C.SRTSOCKET(sock.GetSocket())
	var srterr, syserr C.int
	sent := C.srt_send_batch(u, (*C.char)(unsafe.Pointer(&b[0])), C.long(len(b)),
		C.long(int32(writePollTimeout.Milliseconds())),
		C.long(int32(writeBudget.Milliseconds())),
		&srterr, &syserr)
	return int(sent), srtgo.SRTErrno(int(srterr))
}
