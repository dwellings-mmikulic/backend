package scheduler

import (
	"github.com/dwellingtw/backend/internal/budget"
	"github.com/dwellingtw/backend/internal/bunny"
	"github.com/dwellingtw/backend/internal/hls"
	"github.com/dwellingtw/backend/internal/linear"
	"github.com/dwellingtw/backend/internal/property"
	"github.com/dwellingtw/backend/internal/video"
	"github.com/dwellingtw/backend/internal/workqueue"
	"github.com/dwellingtw/backend/internal/zillow"
	"github.com/dwellingtw/backend/internal/zipcode"
)

// The interfaces in this package are declared by their consumer, so nothing
// but main would notice when one of them drifts from the type that is wired
// into it. These assertions make this package's own build notice.
var (
	_ zillowAPI    = (*zillow.Client)(nil)
	_ zipSource    = (*zipcode.Repository)(nil)
	_ listingQueue = (*workqueue.Repository)(nil)
	_ budgetLedger = (*budget.Ledger)(nil)
	_ windowClock  = (*budget.Windows)(nil)
	_ store        = (*property.Repository)(nil)
	_ uploader     = (*bunny.Client)(nil)
	_ Renderer     = (*video.Renderer)(nil)
	_ Segmenter    = (*hls.Segmenter)(nil)
	_ HLSRecorder  = (*linear.Repository)(nil)
)
