//go:build unix

package httpapi

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentworkspace/internal/platform"
)

type artifactAPI struct{ service *platform.Artifacts }

// ArtifactRoutes is an opt-in local-human read surface, not a member tool entry.
// Downloads retain the server's existing short request and I/O deadlines.
func ArtifactRoutes(service *platform.Artifacts) []Route {
	a := artifactAPI{service}
	return []Route{
		{http.MethodGet, "/api/v1/groups/{group}/artifacts", a.listArtifacts},
		{http.MethodGet, "/api/v1/groups/{group}/artifacts/{artifact}", noQuery(a.getArtifact)},
		{http.MethodGet, "/api/v1/groups/{group}/artifacts/{artifact}/versions", a.listVersions},
		{http.MethodGet, "/api/v1/groups/{group}/artifacts/{artifact}/versions/{version}", noQuery(a.getVersion)},
		{http.MethodGet, "/api/v1/groups/{group}/artifacts/{artifact}/versions/{version}/content", noQuery(a.getContent)},
	}
}

func (a artifactAPI) listArtifacts(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("group")
	options, err := listOptions(r, "artifacts", groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.service.ListArtifacts(r.Context(), groupID, options)
	writePage(w, page, "artifacts", groupID, err)
}

func (a artifactAPI) getArtifact(w http.ResponseWriter, r *http.Request) {
	artifact, err := a.service.GetArtifact(r.Context(), r.PathValue("group"), r.PathValue("artifact"))
	operationResult(w, http.StatusOK, artifact, err)
}

func (a artifactAPI) listVersions(w http.ResponseWriter, r *http.Request) {
	groupID, artifactID := r.PathValue("group"), r.PathValue("artifact")
	// GroupID is a separate cursor field; quoting keeps artifact scope unambiguous.
	scope := "artifact_versions:" + strconv.Quote(artifactID)
	options, err := listOptions(r, scope, groupID)
	if err != nil {
		operationError(w, err)
		return
	}
	page, err := a.service.ListArtifactVersions(r.Context(), groupID, artifactID, options)
	writePage(w, page, scope, groupID, err)
}

func (a artifactAPI) getVersion(w http.ResponseWriter, r *http.Request) {
	version, err := a.service.GetArtifactVersion(r.Context(), r.PathValue("group"), r.PathValue("artifact"), r.PathValue("version"))
	operationResult(w, http.StatusOK, version, err)
}

func (a artifactAPI) getContent(w http.ResponseWriter, r *http.Request) {
	content, err := a.service.OpenArtifactVersion(r.Context(), r.PathValue("group"), r.PathValue("artifact"), r.PathValue("version"))
	if err != nil {
		operationError(w, err)
		return
	}
	defer content.Reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", artifactAttachment(content.Artifact.Name, content.Version.ID))
	// Registry Size is metadata, not verified HTTP framing authority.
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, content.Reader); err != nil {
		// A partial binary response cannot be replaced or completed with JSON.
		panic(http.ErrAbortHandler)
	}
}

func artifactAttachment(name, versionID string) string {
	if !safeArtifactFilename(name) {
		name = versionID
	}
	if !safeArtifactFilename(name) {
		name = "artifact"
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if disposition == "" {
		return "attachment"
	}
	return disposition
}

func safeArtifactFilename(name string) bool {
	// The byte budget also bounds percent-encoding expansion in the header.
	const maxFilenameBytes = 255
	if name == "" || name == "." || name == ".." || len(name) > maxFilenameBytes || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\") {
		return false
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}
