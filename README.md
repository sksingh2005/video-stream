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

You can also upload a video file directly to the processor. This endpoint now queues background processing and returns immediately:

```bash
curl -X POST http://localhost:8080/api/v1/videos/upload \
  -F "videoId=lesson-123" \
  -F "file=@/absolute/path/to/source.mp4"
```

Accepted response:

```json
{
  "job": {
    "jobId": "9f8c0a0d5f4e7b7f1f9d7e90",
    "status": "queued",
    "videoId": "lesson-123",
    "createdAt": "2026-04-29T10:15:00Z"
  }
}
```

Then poll the job status:

```bash
curl http://localhost:8080/api/v1/video-jobs/9f8c0a0d5f4e7b7f1f9d7e90
```

Successful job status:

```json
{
  "job": {
    "jobId": "9f8c0a0d5f4e7b7f1f9d7e90",
    "status": "succeeded",
    "videoId": "lesson-123",
    "createdAt": "2026-04-29T10:15:00Z",
    "startedAt": "2026-04-29T10:15:01Z",
    "completedAt": "2026-04-29T10:18:44Z",
    "result": {
      "videoId": "lesson-123",
      "videoPath": "videos/lesson-123/master.m3u8",
      "thumbnailPath": "videos/lesson-123/thumb.jpg",
      "duration": 812
    }
  }
}
```

For large uploads, the recommended flow is to upload the raw source to R2 first and then enqueue processing from that stored source object:

```bash
curl -X POST http://localhost:8080/api/v1/videos/enqueue \
  -H "Content-Type: application/json" \
  -d '{
    "videoId": "lesson-123",
    "sourceObjectKey": "video-sources/content-123/raw.mp4",
    "cleanupSourceObject": true
  }'
```

This avoids proxying a 300-400 MB source file through your LMS app and then through the Go processor again. After a successful publish, the processor deletes the temporary raw source object when `cleanupSourceObject` is `true`.

## Integration files

Drop these into `edtech-lms-platform`:

- `examples/edtech-lms-platform/prisma/migration.sql`
- `examples/edtech-lms-platform/src/lib/hls-playback.ts`
- `examples/edtech-lms-platform/src/app/api/learning/content/[contentId]/playback/route.ts`

Set this in the LMS environment so Next.js can call the Go processor:

```env
VIDEO_PROCESSOR_API_URL=http://localhost:8080
```

Then the LMS admin route should:

1. Create a presigned R2 upload URL for a temporary source object such as `video-sources/content-123/...`
2. Upload the raw source file directly from the browser to R2
3. `POST` the source object key to `/api/v1/videos/enqueue`
4. Store the returned `jobId`
5. Poll `/api/v1/video-jobs/:jobId`
6. Persist the HLS paths only after `status === "succeeded"`

If your LMS prefers to upload the file itself and send a server path later, `/api/v1/videos/process` remains available for synchronous server-side processing.

Example of the final result payload the LMS should store after the job succeeds:

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
