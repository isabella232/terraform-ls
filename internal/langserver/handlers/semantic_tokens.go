package handlers

import (
	"context"
	"fmt"

	lsctx "github.com/hashicorp/terraform-ls/internal/context"
	ilsp "github.com/hashicorp/terraform-ls/internal/lsp"
	lsp "github.com/hashicorp/terraform-ls/internal/protocol"
)

func (lh *logHandler) TextDocumentSemanticTokensFull(ctx context.Context, params lsp.SemanticTokensParams) (lsp.SemanticTokens, error) {
	tks := lsp.SemanticTokens{}

	cc, err := lsctx.ClientCapabilities(ctx)
	if err != nil {
		return tks, err
	}

	ds, err := lsctx.DocumentStorage(ctx)
	if err != nil {
		return tks, err
	}

	rmf, err := lsctx.RootModuleFinder(ctx)
	if err != nil {
		return tks, err
	}

	fh := ilsp.FileHandlerFromDocumentURI(params.TextDocument.URI)
	doc, err := ds.GetDocument(fh)
	if err != nil {
		return tks, err
	}

	rm, err := rmf.RootModuleByPath(doc.Dir())
	if err != nil {
		return tks, fmt.Errorf("finding compatible decoder failed: %w", err)
	}

	schema, err := rmf.SchemaForPath(doc.Dir())
	if err != nil {
		return tks, err
	}

	d, err := rm.DecoderWithSchema(schema)
	if err != nil {
		return tks, err
	}

	tokens, err := d.SemanticTokensInFile(doc.Filename())
	if err != nil {
		return tks, err
	}

	te := &ilsp.TokenEncoder{
		Lines:      doc.Lines(),
		Tokens:     tokens,
		ClientCaps: cc.TextDocument.SemanticTokens,
	}
	tks.Data = te.Encode()

	return tks, nil
}
