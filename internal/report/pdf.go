package report

import (
	"errors"
	"io"
)

func newPDF(w io.Writer) (writer, error) { return nil, errors.New("pdf: not yet") }
