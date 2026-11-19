package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// S3 multipart upload (CreateMultipartUpload / UploadPart / ListParts /
// CompleteMultipartUpload / AbortMultipartUpload), signed with the same
// query-string SigV4 as everything else.

func s3Err(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("s3 %s: %s: %s", op, resp.Status, bytes.TrimSpace(b))
}

func (s *S3) CreateMultipart(ctx context.Context, key string) (string, error) {
	resp, err := s.doQ(ctx, http.MethodPost, key, map[string]string{"uploads": ""}, nil, -1)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", s3Err("create multipart", resp)
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("s3 create multipart: %w", err)
	}
	if out.UploadID == "" {
		return "", fmt.Errorf("s3 create multipart: no UploadId")
	}
	return out.UploadID, nil
}

func partQuery(uploadID string, n int) map[string]string {
	return map[string]string{"partNumber": strconv.Itoa(n), "uploadId": uploadID}
}

func (s *S3) PartURL(_ context.Context, key, uploadID string, n int, ttl time.Duration) (string, error) {
	return s.presign(http.MethodPut, key, ttl, partQuery(uploadID, n))
}

func (s *S3) PutPart(ctx context.Context, key, uploadID string, n int, r io.Reader, size int64) error {
	resp, err := s.doQ(ctx, http.MethodPut, key, partQuery(uploadID, n), r, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return s3Err("upload part", resp)
	}
	return nil
}

type s3ListParts struct {
	IsTruncated          bool `xml:"IsTruncated"`
	NextPartNumberMarker int  `xml:"NextPartNumberMarker"`
	Parts                []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
		Size       int64  `xml:"Size"`
	} `xml:"Part"`
}

func (s *S3) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	var out []Part
	marker := 0
	for range 100 { // 100 pages x 1000 parts > the 10,000-part limit
		q := map[string]string{"uploadId": uploadID}
		if marker > 0 {
			q["part-number-marker"] = strconv.Itoa(marker)
		}
		resp, err := s.doQ(ctx, http.MethodGet, key, q, nil, -1)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return nil, ErrNotFound
		}
		if resp.StatusCode/100 != 2 {
			err := s3Err("list parts", resp)
			resp.Body.Close()
			return nil, err
		}
		var page s3ListParts
		err = xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3 list parts: %w", err)
		}
		for _, p := range page.Parts {
			out = append(out, Part{N: p.PartNumber, Size: p.Size, ETag: p.ETag})
		}
		if !page.IsTruncated || page.NextPartNumberMarker <= marker {
			break
		}
		marker = page.NextPartNumberMarker
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

type s3CompletePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type s3Complete struct {
	XMLName xml.Name         `xml:"CompleteMultipartUpload"`
	Parts   []s3CompletePart `xml:"Part"`
}

func (s *S3) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	body := s3Complete{}
	for _, p := range parts {
		body.Parts = append(body.Parts, s3CompletePart{PartNumber: p.N, ETag: p.ETag})
	}
	b, err := xml.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := s.doQ(ctx, http.MethodPost, key, map[string]string{"uploadId": uploadID}, bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return s3Err("complete multipart", resp)
	}
	// S3 can answer 200 and still report an error in the body.
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if bytes.Contains(rb, []byte("<Error>")) {
		return fmt.Errorf("s3 complete multipart: %s", bytes.TrimSpace(rb))
	}
	return nil
}

func (s *S3) AbortMultipart(ctx context.Context, key, uploadID string) error {
	resp, err := s.doQ(ctx, http.MethodDelete, key, map[string]string{"uploadId": uploadID}, nil, -1)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("s3 abort multipart: %s", resp.Status)
	}
	return nil
}
