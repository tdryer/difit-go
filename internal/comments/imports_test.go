package comments

import (
	"strings"
	"testing"

	"github.com/tdryer/difit-go/internal/types"
)

func thread(id, body, updatedAt string) types.DiffCommentThread {
	return types.DiffCommentThread{
		ID:        id,
		FilePath:  "src/example.ts",
		CreatedAt: "2024-01-01T00:00:00.000Z",
		UpdatedAt: updatedAt,
		Position:  types.DiffCommentPosition{Side: "new", Line: types.SingleLine(10)},
		Messages: []types.DiffCommentMessage{{
			ID: id, Body: body, Author: types.Ptr("User"),
			CreatedAt: "2024-01-01T00:00:00.000Z", UpdatedAt: updatedAt,
		}},
	}
}

func importOf(typ, body string) types.CommentImport {
	return types.CommentImport{
		Type:     typ,
		FilePath: "src/example.ts",
		Position: types.DiffCommentPosition{Side: "new", Line: types.SingleLine(10)},
		Body:     body,
	}
}

func TestMergeImportsAddsThread(t *testing.T) {
	result := MergeImports(nil, []types.CommentImport{importOf("thread", "Imported thread")})
	if len(result.Warnings) != 0 || len(result.Threads) != 1 {
		t.Fatalf("got %+v", result)
	}
	th := result.Threads[0]
	if th.Messages[0].Body != "Imported thread" || th.Messages[0].ID != th.ID || len(th.ID) != 16 {
		t.Fatalf("unexpected thread %+v", th)
	}
}

func TestMergeImportsSkipsDuplicateThread(t *testing.T) {
	ci := importOf("thread", "Imported thread")
	ci.Author = types.Ptr("User")
	result := MergeImports([]types.DiffCommentThread{thread("thread-1", "Imported thread", "2024-01-01T00:00:00.000Z")}, []types.CommentImport{ci})
	if len(result.Threads) != 1 {
		t.Fatalf("expected duplicate to be skipped, got %d threads", len(result.Threads))
	}
}

func TestMergeImportsRepliesToNewestThread(t *testing.T) {
	existing := []types.DiffCommentThread{
		thread("thread-1", "Root 1", "2024-01-01T00:00:00.000Z"),
		thread("thread-2", "Root 2", "2024-01-02T00:00:00.000Z"),
	}
	reply := importOf("reply", "Imported reply")
	reply.Author = types.Ptr("AI")
	result := MergeImports(existing, []types.CommentImport{reply})
	if len(result.Threads[0].Messages) != 1 || len(result.Threads[1].Messages) != 2 {
		t.Fatalf("reply attached to wrong thread: %+v", result.Threads)
	}
	if len(existing[1].Messages) != 1 {
		t.Fatal("input threads must not be mutated")
	}
}

func TestMergeImportsSkipsDuplicateReply(t *testing.T) {
	existing := thread("thread-1", "Root", "2024-01-01T00:00:00.000Z")
	existing.Messages = append(existing.Messages, types.DiffCommentMessage{
		ID: "reply-1", Body: "Imported reply", Author: types.Ptr("AI"),
		CreatedAt: "2024-01-02T00:00:00.000Z", UpdatedAt: "2024-01-02T00:00:00.000Z",
	})
	reply := importOf("reply", "Imported reply")
	reply.ID = types.Ptr("reply-1")
	result := MergeImports([]types.DiffCommentThread{existing}, []types.CommentImport{reply})
	if len(result.Threads[0].Messages) != 2 {
		t.Fatalf("expected duplicate reply skipped, got %d messages", len(result.Threads[0].Messages))
	}
}

func TestMergeImportsWarnsOnOrphanReply(t *testing.T) {
	result := MergeImports(nil, []types.CommentImport{importOf("reply", "Imported reply")})
	if len(result.Threads) != 0 || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "Skipped reply import for src/example.ts:new:10") {
		t.Fatalf("got %+v", result)
	}
}

func TestMergeThreadsCombinesConcurrentEdits(t *testing.T) {
	base := thread("t1", "Root", "2024-01-01T00:00:00.000Z")

	ours := base
	ours.Messages = append([]types.DiffCommentMessage{}, base.Messages...)
	ours.Messages = append(ours.Messages, types.DiffCommentMessage{ID: "b", Body: "second", CreatedAt: "2024-01-03T00:00:00.000Z", UpdatedAt: "2024-01-03T00:00:00.000Z"})

	theirs := base
	theirs.Messages = append([]types.DiffCommentMessage{}, base.Messages...)
	theirs.Messages = append(theirs.Messages, types.DiffCommentMessage{ID: "a", Body: "first", CreatedAt: "2024-01-02T00:00:00.000Z", UpdatedAt: "2024-01-02T00:00:00.000Z"})
	theirs.Messages[0].Body = "Root edited"
	theirs.Messages[0].UpdatedAt = "2024-01-04T00:00:00.000Z"

	other := thread("t2", "Other", "2024-01-01T00:00:00.000Z")
	other.Position.Line = types.SingleLine(20)

	result := MergeThreads([]types.DiffCommentThread{ours}, []types.DiffCommentThread{theirs, other})
	if len(result.Threads) != 2 {
		t.Fatalf("expected 2 threads, got %d", len(result.Threads))
	}
	msgs := result.Threads[0].Messages
	if len(msgs) != 3 || msgs[0].Body != "Root edited" || msgs[1].ID != "a" || msgs[2].ID != "b" {
		t.Fatalf("unexpected merged messages %+v", msgs)
	}
	if result.Threads[0].UpdatedAt != "2024-01-04T00:00:00.000Z" {
		t.Fatalf("updatedAt = %s", result.Threads[0].UpdatedAt)
	}
}
