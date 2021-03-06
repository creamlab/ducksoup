package front

import (
	"github.com/creamlab/ducksoup/helpers"
	"github.com/evanw/esbuild/pkg/api"
	"github.com/rs/zerolog/log"
)

var (
	developmentMode bool = false
	cmdBuildMode    bool = false
)

func init() {
	if helpers.Getenv("DS_ENV") == "DEV" {
		developmentMode = true
	}
	if helpers.Getenv("DS_ENV") == "BUILD_FRONT" {
		cmdBuildMode = true
	}
}

// API

func Build() {
	// only build in certain conditions (= not when launching ducksoup in production)
	if developmentMode || cmdBuildMode {
		buildOptions := api.BuildOptions{
			EntryPoints:       []string{"front/src/lib/ducksoup.js", "front/src/test/play/app.jsx", "front/src/test/mirror/app.js", "front/src/stats/app.js"},
			Bundle:            true,
			MinifyWhitespace:  !developmentMode,
			MinifyIdentifiers: !developmentMode,
			MinifySyntax:      !developmentMode,
			Engines: []api.Engine{
				{api.EngineChrome, "64"},
				{api.EngineFirefox, "53"},
				{api.EngineSafari, "11"},
				{api.EngineEdge, "79"},
			},
			Outdir: "front/static/assets/scripts",
			Write:  true,
		}
		if developmentMode {
			buildOptions.Watch = &api.WatchMode{
				OnRebuild: func(result api.BuildResult) {
					if len(result.Errors) > 0 {
						for _, msg := range result.Errors {
							log.Error().Str("context", "js_build").Msg(msg.Text)
						}
					} else {
						if len(result.Warnings) > 0 {
							for _, msg := range result.Warnings {
								log.Info().Str("context", "js_build").Msgf("%v", msg.Text)
							}
						} else {
							log.Info().Str("context", "js_build").Msg("build_success")
						}
					}
				},
			}
		}
		build := api.Build(buildOptions)

		if len(build.Errors) > 0 {
			log.Fatal().Str("context", "js_build").Msgf("%v", build.Errors[0].Text)
		}
	}
}
