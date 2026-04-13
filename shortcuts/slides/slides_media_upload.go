// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package slides

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/larksuite/cli/extension/fileio"
	"github.com/larksuite/cli/internal/output"
	"github.com/larksuite/cli/shortcuts/common"
)

// slidesMediaParentType is the only parent_type the slides backend accepts for
// media uploaded against an xml_presentation. Verified empirically:
// `slide_image` returns 1061001 unknown error, `slides_image` / `slides_file`
// return 1061002 params error, but `slide_file` returns a valid file_token
// that can be used as <img src="..."> in slide XML.
const slidesMediaParentType = "slide_file"

// SlidesMediaUpload uploads a local image to drive media against a slides
// presentation and returns the file_token. The token can be used as the value
// of <img src="..."> in slide XML.
//
// This is the atomic building block for getting a local image into a slides
// deck. Higher-level shortcuts (e.g. +create with @path placeholders) reuse
// the same upload helpers.
var SlidesMediaUpload = common.Shortcut{
	Service:     "slides",
	Command:     "+media-upload",
	Description: "Upload a local image to a slides presentation and return the file_token (use as <img src=...>)",
	Risk:        "write",
	// wiki:node:read is only needed when --presentation is a /wiki/ URL; checked
	// at runtime in Validate so callers using slides URLs / raw tokens don't
	// have to grant a wiki scope they never use.
	Scopes:    []string{"docs:document.media:upload"},
	AuthTypes: []string{"user", "bot"},
	Flags: []common.Flag{
		{Name: "file", Desc: "local image path (files > 20MB use multipart upload automatically)", Required: true},
		{Name: "presentation", Desc: "xml_presentation_id, slides URL, or wiki URL that resolves to slides", Required: true},
	},
	Validate: func(ctx context.Context, runtime *common.RuntimeContext) error {
		ref, err := parsePresentationRef(runtime.Str("presentation"))
		if err != nil {
			return err
		}
		if ref.Kind == "wiki" {
			if err := runtime.RequireScopes("wiki:node:read"); err != nil {
				return err
			}
		}
		return nil
	},
	DryRun: func(ctx context.Context, runtime *common.RuntimeContext) *common.DryRunAPI {
		filePath := runtime.Str("file")
		ref, err := parsePresentationRef(runtime.Str("presentation"))
		if err != nil {
			return common.NewDryRunAPI().Set("error", err.Error())
		}

		dry := common.NewDryRunAPI()
		parentNode := ref.Token
		stepBase := 1
		if ref.Kind == "wiki" {
			parentNode = "<resolved_slides_token>"
			stepBase = 2
			dry.Desc("2-step orchestration: resolve wiki → upload media").
				GET("/open-apis/wiki/v2/spaces/get_node").
				Desc("[1] Resolve wiki node to slides presentation").
				Params(map[string]interface{}{"token": ref.Token})
		} else {
			dry.Desc("Upload local file to slides presentation")
		}
		appendSlidesUploadDryRun(dry, runtime.FileIO(), filePath, parentNode, stepBase)
		return dry.Set("presentation_id", ref.Token)
	},
	Execute: func(ctx context.Context, runtime *common.RuntimeContext) error {
		filePath := runtime.Str("file")
		ref, err := parsePresentationRef(runtime.Str("presentation"))
		if err != nil {
			return err
		}
		presentationID, err := resolvePresentationID(runtime, ref)
		if err != nil {
			return err
		}

		stat, err := runtime.FileIO().Stat(filePath)
		if err != nil {
			return common.WrapInputStatError(err, "file not found")
		}
		if !stat.Mode().IsRegular() {
			return output.ErrValidation("file must be a regular file: %s", filePath)
		}

		fileName := filepath.Base(filePath)
		fmt.Fprintf(runtime.IO().ErrOut, "Uploading: %s (%s) -> presentation %s\n",
			fileName, common.FormatSize(stat.Size()), common.MaskToken(presentationID))
		if stat.Size() > common.MaxDriveMediaUploadSinglePartSize {
			fmt.Fprintf(runtime.IO().ErrOut, "File exceeds 20MB, using multipart upload\n")
		}

		fileToken, err := uploadSlidesMedia(runtime, filePath, fileName, stat.Size(), presentationID)
		if err != nil {
			return err
		}

		runtime.Out(map[string]interface{}{
			"file_token":      fileToken,
			"file_name":       fileName,
			"size":            stat.Size(),
			"presentation_id": presentationID,
		}, nil)
		return nil
	},
}

// uploadSlidesMedia is the shared upload helper used by both +media-upload and
// the +create placeholder pipeline. Always uses parent_type=slide_file with the
// presentation_id as parent_node — verified to be the only working combo.
func uploadSlidesMedia(runtime *common.RuntimeContext, filePath, fileName string, fileSize int64, presentationID string) (string, error) {
	if fileSize <= common.MaxDriveMediaUploadSinglePartSize {
		parent := presentationID
		return common.UploadDriveMediaAll(runtime, common.DriveMediaUploadAllConfig{
			FilePath:   filePath,
			FileName:   fileName,
			FileSize:   fileSize,
			ParentType: slidesMediaParentType,
			ParentNode: &parent,
		})
	}
	return common.UploadDriveMediaMultipart(runtime, common.DriveMediaMultipartUploadConfig{
		FilePath:   filePath,
		FileName:   fileName,
		FileSize:   fileSize,
		ParentType: slidesMediaParentType,
		ParentNode: presentationID,
	})
}

// appendSlidesUploadDryRun renders the upload steps for a single file, choosing
// single-part vs multipart based on local stat (best-effort planning hint).
func appendSlidesUploadDryRun(d *common.DryRunAPI, fio fileio.FileIO, filePath, parentNode string, step int) {
	if slidesUploadShouldUseMultipart(fio, filePath) {
		d.POST("/open-apis/drive/v1/medias/upload_prepare").
			Desc(fmt.Sprintf("[%da] Initialize multipart upload", step)).
			Body(map[string]interface{}{
				"file_name":   filepath.Base(filePath),
				"parent_type": slidesMediaParentType,
				"parent_node": parentNode,
				"size":        "<file_size>",
			}).
			POST("/open-apis/drive/v1/medias/upload_part").
			Desc(fmt.Sprintf("[%db] Upload file parts (repeated)", step)).
			Body(map[string]interface{}{
				"upload_id": "<upload_id>",
				"seq":       "<chunk_index>",
				"size":      "<chunk_size>",
				"file":      "<chunk_binary>",
			}).
			POST("/open-apis/drive/v1/medias/upload_finish").
			Desc(fmt.Sprintf("[%dc] Finalize multipart upload and get file_token", step)).
			Body(map[string]interface{}{
				"upload_id": "<upload_id>",
				"block_num": "<block_num>",
			})
		return
	}

	d.POST("/open-apis/drive/v1/medias/upload_all").
		Desc(fmt.Sprintf("[%d] Upload local file (multipart/form-data)", step)).
		Body(map[string]interface{}{
			"file_name":   filepath.Base(filePath),
			"parent_type": slidesMediaParentType,
			"parent_node": parentNode,
			"size":        "<file_size>",
			"file":        "@" + filePath,
		})
}

func slidesUploadShouldUseMultipart(fio fileio.FileIO, filePath string) bool {
	info, err := fio.Stat(filePath)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Size() > common.MaxDriveMediaUploadSinglePartSize
}
