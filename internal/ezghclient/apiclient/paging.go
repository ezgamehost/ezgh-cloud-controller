package apiclient

import "context"

// Page is one page of a list.
type Page[T any] struct {
	Items      []T
	NextCursor *string
}

// Paginate reads a list page by page: one page (starting at cursor) unless all is set, in which
// case it follows nextCursor to the end. Clients follow nextCursor, never count rows: a page can
// be short and still have a next one (contracts/api-conventions.md#pagination). each gets every
// page as it arrives; Paginate returns the cursor after the last page read ("" at the end).
func Paginate[T any](ctx context.Context, cursor string, all bool, fetch func(ctx context.Context, cursor string) (Page[T], error), each func([]T) error) (string, error) {
	for {
		page, err := fetch(ctx, cursor)
		if err != nil {
			return "", err
		}
		if err := each(page.Items); err != nil {
			return "", err
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return "", nil
		}
		cursor = *page.NextCursor
		if !all {
			return cursor, nil
		}
	}
}
