package envoy_mcp_openapi_processor

import (
	"bytes"
	"fmt"
	"testing"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrame(t *testing.T) {
	t.Parallel()

	const s = streamChunkSize

	// expectedChunks is ceil(len/s), but an empty body still yields one chunk.
	expectedChunks := func(n int) int {
		return max(1, (n+s-1)/s)
	}

	sizes := []int{0, 1, s - 1, s, s + 1, 2 * s, 2*s + 1}

	for _, size := range sizes {
		for _, hasTrailers := range []bool{false, true} {
			size, hasTrailers := size, hasTrailers
			t.Run(fmt.Sprintf("size=%d/trailers=%v", size, hasTrailers), func(t *testing.T) {
				t.Parallel()

				body := make([]byte, size)
				for i := range body {
					// fill with abcdef...xyz pattern
					body[i] = byte('a' + i%26)
				}
				headers := newResponseHeaders(&extProcPb.CommonResponse{})

				reps := frame(streamedMutation{headers: headers, body: body}, responseFactory, hasTrailers)

				// First response is the header mutation, verbatim.
				require.NotEmpty(t, reps)
				assert.Same(t, headers, reps[0], "first response must be the header mutation")

				var (
					chunks   []*extProcPb.StreamedBodyResponse
					trailers int
				)
				for _, rep := range reps[1:] {
					if rb := rep.GetResponseBody(); rb != nil {
						streamed := rb.GetResponse().GetBodyMutation().GetStreamedResponse()
						require.NotNil(t, streamed, "body mutation must be streamed")
						chunks = append(chunks, streamed)
						continue
					}
					if rep.GetResponseTrailers() != nil {
						trailers++
						assert.Nil(t, rep.GetResponseTrailers().GetHeaderMutation(),
							"TrailersResponse must carry no mutation")
						continue
					}
					t.Fatalf("unexpected response in framed output: %T", rep.GetResponse())
				}

				// Chunk count and size cap.
				assert.Len(t, chunks, expectedChunks(size))
				for i, chunk := range chunks {
					assert.LessOrEqual(t, len(chunk.GetBody()), s, "chunk %d exceeds max size", i)
				}

				// Reassembled chunks reproduce the original body.
				var got []byte
				for _, chunk := range chunks {
					got = append(got, chunk.GetBody()...)
				}
				assert.True(t, bytes.Equal(body, got), "reassembled body must match input")

				// EOS and trailer semantics.
				if hasTrailers {
					assert.Equal(t, 1, trailers, "trailers must be acknowledged exactly once")
					for i, chunk := range chunks {
						assert.False(t, chunk.GetEndOfStream(),
							"chunk %d must not set end_of_stream when trailers are present", i)
					}
				} else {
					assert.Zero(t, trailers, "no trailer message without trailers")
					for i, chunk := range chunks {
						want := i == len(chunks)-1
						assert.Equal(t, want, chunk.GetEndOfStream(),
							"chunk %d end_of_stream: only the final chunk sets it", i)
					}
				}
			})
		}
	}
}
