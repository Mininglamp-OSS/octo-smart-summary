package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// TokenResolver resolves a token string to a user ID.
type TokenResolver interface {
	ResolveUID(ctx context.Context, token string) (string, error)
}

type BotIdentity struct {
	BotUID   string
	OwnerUID string
	SpaceID  string
}

type BotTokenResolver interface {
	ResolveBot(ctx context.Context, token string) (BotIdentity, error)
}

// StrictBotAuthMiddleware authenticates the bot realm and injects the bot's
// human owner as the effective reader. Space is exclusively server-resolved.
func StrictBotAuthMiddleware(resolver BotTokenResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		parts := strings.Fields(strings.TrimSpace(c.GetHeader("Authorization")))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !strings.HasPrefix(parts[1], "bf_") || resolver == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "authentication required"})
			return
		}
		identity, err := resolver.ResolveBot(c.Request.Context(), parts[1])
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"code": 5001, "message": "bot token resolution error"})
			return
		}
		if identity.BotUID == "" || identity.OwnerUID == "" || identity.SpaceID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 4010, "message": "invalid or expired bot token"})
			return
		}
		c.Set("actor_type", "bot")
		c.Set("actor_id", identity.BotUID)
		c.Set("bot_id", identity.BotUID)
		c.Set("user_id", identity.OwnerUID)
		c.Set("space_id", identity.SpaceID)
		c.Set("bot_request", true)
		c.Next()
	}
}

// SpaceMiddleware extracts X-Space-Id (or X-Org-Id for backward compat) header
// and sets it in context. Allows empty space_id (matches Python prototype behaviour
// where X-Org-Id defaults to empty string).
func SpaceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID := c.GetHeader("X-Space-Id")
		if spaceID == "" {
			// Fallback to X-Org-Id for backward compatibility
			spaceID = c.GetHeader("X-Org-Id")
		}
		// Allow empty space_id — DB queries will use empty string scope (no isolation).
		c.Set("space_id", spaceID)
		c.Next()
	}
}

// GetSpaceID retrieves space_id from gin context.
func GetSpaceID(c *gin.Context) string {
	v, _ := c.Get("space_id")
	s, _ := v.(string)
	return s
}

// AuthMiddleware extracts Token header, resolves uid, and sets user_id in context.
// Allows unauthenticated requests through with empty user_id.
func AuthMiddleware(resolver TokenResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Token")
		userID := ""

		if token != "" && resolver != nil {
			uid, err := resolver.ResolveUID(c.Request.Context(), token)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":    5001,
					"message": "token resolution error",
				})
				return
			}
			if uid == "" {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"code":    4010,
					"message": "invalid or expired token",
				})
				return
			}
			userID = uid
		}

		// Allow requests without credentials (matches Python prototype behaviour);
		// user_id will be empty string if not authenticated.

		c.Set("user_id", userID)
		c.Set("token", token)
		c.Next()
	}
}

// GetUserID retrieves user_id from gin context.
func GetUserID(c *gin.Context) string {
	v, _ := c.Get("user_id")
	s, _ := v.(string)
	return s
}

// StrictAuthMiddleware requires a valid token that resolves to a user_id.
// Returns 401 if user_id is empty after token resolution.
func StrictAuthMiddleware(resolver TokenResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Token")
		userID := ""

		if token != "" && resolver != nil {
			uid, err := resolver.ResolveUID(c.Request.Context(), token)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":    5001,
					"message": "token resolution error",
				})
				return
			}
			if uid == "" {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"code":    4010,
					"message": "invalid or expired token",
				})
				return
			}
			userID = uid
		}

		if userID == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "authentication required",
			})
			return
		}

		c.Set("user_id", userID)
		c.Set("token", token)
		c.Next()
	}
}

// StrictSpaceMiddleware extracts X-Space-Id and rejects write operations without it.
func StrictSpaceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID := c.GetHeader("X-Space-Id")
		if spaceID == "" {
			spaceID = c.GetHeader("X-Org-Id")
		}

		if spaceID == "" && isWriteMethod(c.Request.Method) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":    40001,
				"message": "X-Space-Id header required for write operations",
			})
			return
		}

		c.Set("space_id", spaceID)
		c.Next()
	}
}

func isWriteMethod(method string) bool {
	return method == "POST" || method == "PUT" || method == "DELETE"
}
