package storage

import (
	"mime"
	"path/filepath"
	"strings"
)

func DetectContentType(filePath string) string {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".m4s":
		return "video/iso.segment"
	case ".mp4":
		return "video/mp4"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".vtt":
		return "text/vtt"
	default:
		contentType := mime.TypeByExtension(filepath.Ext(filePath))
		if contentType == "" {
			return "application/octet-stream"
		}
		return contentType
	}
}
