package oss

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

type movedObject struct {
	src  string
	dst  string
	data []byte
}

// MovePrefixVerified implements the lossless prefix move contract using the
// primitive StorageClient operations. It copies and byte-verifies every
// source object before deleting any source. If a later delete fails, already
// deleted objects are restored from their verified copies before returning.
func MovePrefixVerified(ctx context.Context, c StorageClient, srcPrefix, dstPrefix string) error {
	src := strings.TrimSuffix(strings.TrimSpace(srcPrefix), "/") + "/"
	dst := strings.TrimSuffix(strings.TrimSpace(dstPrefix), "/") + "/"
	if src == "/" || dst == "/" {
		return fmt.Errorf("source and destination prefixes are required")
	}
	if src == dst {
		return fmt.Errorf("source and destination prefixes must differ")
	}

	keys, err := c.ListObjects(ctx, src)
	if err != nil {
		return fmt.Errorf("list source prefix %s: %w", src, err)
	}
	copied := make([]movedObject, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(key, src) {
			return fmt.Errorf("storage listed object %q outside source prefix %q", key, src)
		}
		data, err := c.GetObject(ctx, key)
		if err != nil {
			return fmt.Errorf("read source object %s: %w", key, err)
		}
		dstKey := dst + strings.TrimPrefix(key, src)
		if err := c.PutObject(ctx, dstKey, data); err != nil {
			return fmt.Errorf("copy %s to %s: %w", key, dstKey, err)
		}
		verified, err := c.GetObject(ctx, dstKey)
		if err != nil {
			return fmt.Errorf("verify destination object %s: %w", dstKey, err)
		}
		if !bytes.Equal(data, verified) {
			return fmt.Errorf("verify destination object %s: content mismatch", dstKey)
		}
		copied = append(copied, movedObject{src: key, dst: dstKey, data: data})
	}

	for i := range copied {
		if err := c.DeleteObject(ctx, copied[i].src); err != nil && !errors.Is(err, os.ErrNotExist) {
			restoreErrs := []error{fmt.Errorf("delete source object %s: %w", copied[i].src, err)}
			for j := 0; j < i; j++ {
				if restoreErr := c.PutObject(ctx, copied[j].src, copied[j].data); restoreErr != nil {
					restoreErrs = append(restoreErrs, fmt.Errorf("restore source object %s: %w", copied[j].src, restoreErr))
				}
			}
			return errors.Join(restoreErrs...)
		}
	}
	return nil
}
