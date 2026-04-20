# Secure HLS on Private R2

This repo contains a Go video-processing service plus integration artifacts for a secure HLS playback stack built on:

- Go + FFmpeg for transcoding
- Cloudflare R2 for private storage
- Cloudflare Worker for token-protected delivery
- Next.js for auth, access checks, and playback URL issuance

## Go service

Run the API:

```bash
go run ./cmd/server
```

Process a source video:

```bash
curl -X POST http://localhost:8080/api/v1/videos/process \
  -H "Content-Type: application/json" \
  -d '{
    "videoId": "lesson-123",
    "sourcePath": "/absolute/path/to/source.mp4",
    "cleanupSource": true
  }'
```

Successful response:

```json
{
  "videoId": "lesson-123",
  "videoPath": "videos/lesson-123/master.m3u8",
  "thumbnailPath": "videos/lesson-123/thumb.jpg",
  "duration": 812,
  "variants": [
    {
      "name": "720p",
      "playlist": "videos/lesson-123/720p/index.m3u8",
      "width": 1280,
      "height": 720
    }
  ]
}
```

The service never returns public R2 links. It uploads the full HLS tree to your private bucket and returns only storage paths.

If `cleanupSource` is `true`, the original local source file is deleted only after every generated HLS asset has been uploaded and verified in R2 with a matching object size.

You can also upload a video file directly to the processor:

```bash
curl -X POST http://localhost:8080/api/v1/videos/upload \
  -F "videoId=lesson-123" \
  -F "file=@/absolute/path/to/source.mp4"
```

The LMS admin video upload now uses this multipart endpoint through Next.js, so the browser no longer sends video files directly to R2 and does not need R2 CORS for video uploads.

## Integration files

Drop these into `edtech-lms-platform`:

- `examples/edtech-lms-platform/prisma/migration.sql`
- `examples/edtech-lms-platform/src/lib/hls-playback.ts`
- `examples/edtech-lms-platform/src/app/api/learning/content/[contentId]/playback/route.ts`

Set this in the LMS environment so Next.js can call the Go processor:

```env
VIDEO_PROCESSOR_API_URL=http://localhost:8080
```

Then the LMS admin route can call the processor and store the returned HLS path:

```json
{
  "action": "process_hls",
  "videoId": "lesson-123",
  "sourcePath": "D:\\uploads\\lesson-123.mp4",
  "cleanupSource": true
}
```

Deploy this to Cloudflare:

- `examples/cloudflare-worker/src/index.ts`
- `examples/cloudflare-worker/wrangler.toml`

## Security model

- R2 stays private.
- PostgreSQL stores only HLS object paths.
- Next.js issues short-lived HMAC playback tokens after auth and course-access checks.
- Cloudflare Worker validates the token on every request.
- `.m3u8` playlists are rewritten so child manifests and media segments inherit the token.
