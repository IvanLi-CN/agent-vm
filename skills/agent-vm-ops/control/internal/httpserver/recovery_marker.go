package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
)

var recoveryMarkerID = regexp.MustCompile(`^point-[a-f0-9]{32}$`)

func (s *Server) recoveryMarker(c *gin.Context) {
	if _, ok := c.Request.Context().Value(peerContextKey{}).(*peerConn); !ok || s.RoleGate == nil {
		c.JSON(http.StatusForbidden, gin.H{"ok": false})
		return
	}
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, 256)
	defer reader.Close()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var request struct {
		ID string `json:"id"`
	}
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || !recoveryMarkerID.MatchString(request.ID) {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false})
		return
	}
	if err := s.RoleGate.WithValid(func() error { return s.Service.Store.WriteRecoveryMarker(c.Request.Context(), request.ID) }); err != nil {
		c.JSON(http.StatusConflict, gin.H{"ok": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
