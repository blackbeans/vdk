package format

import (
	"github.com/blackbeans/vdk/av/avutil"
	"github.com/blackbeans/vdk/format/aac"
	"github.com/blackbeans/vdk/format/flv"
	"github.com/blackbeans/vdk/format/mp4"
	"github.com/blackbeans/vdk/format/rtmp"
	"github.com/blackbeans/vdk/format/rtsp"
	"github.com/blackbeans/vdk/format/ts"
)

func RegisterAll() {
	avutil.DefaultHandlers.Add(mp4.Handler)
	avutil.DefaultHandlers.Add(ts.Handler)
	avutil.DefaultHandlers.Add(rtmp.Handler)
	avutil.DefaultHandlers.Add(rtsp.Handler)
	avutil.DefaultHandlers.Add(flv.Handler)
	avutil.DefaultHandlers.Add(aac.Handler)
}
