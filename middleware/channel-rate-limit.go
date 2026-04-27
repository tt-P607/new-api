package middleware

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
)

const (
	ChannelRateLimitMark = "CRL"
)

// ChannelRateLimit 每渠道用户速率限制中间件。
// 必须在 Distribute() 之后注册，因为需要从 context 中读取已选渠道的 OtherSettings。
func ChannelRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取渠道的 OtherSettings
		otherSettingsVal, exists := common.GetContextKey(c, constant.ContextKeyChannelOtherSetting)
		if !exists {
			c.Next()
			return
		}
		otherSettings, ok := otherSettingsVal.(dto.ChannelOtherSettings)
		if !ok {
			c.Next()
			return
		}

		channelId := common.GetContextKeyInt(c, constant.ContextKeyChannelId)
		userId := c.GetInt("id")
		userRole := c.GetInt("role")

		// ── 渠道级别速率限制 ──────────────────────────────────────────
		if otherSettings.ChannelRateLimitEnabled && otherSettings.ChannelRateLimitCount > 0 {
			if otherSettings.ChannelRateLimitLimitAdmins || userRole < common.RoleAdminUser {
				if channelId > 0 && userId > 0 {
					key := fmt.Sprintf("%s:ch:%d:%d", ChannelRateLimitMark, channelId, userId)
					durationMinutes := otherSettings.ChannelRateLimitDuration
					if durationMinutes <= 0 {
						durationMinutes = 1
					}
					duration := int64(durationMinutes * 60)
					maxCount := otherSettings.ChannelRateLimitCount

					var allowed bool
					if common.RedisEnabled {
						allowed = channelRedisRateLimit(key, maxCount, duration, durationMinutes)
					} else {
						inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
						allowed = inMemoryRateLimiter.Request(key, maxCount, duration)
					}
					if !allowed {
						abortWithOpenAiMessage(c, http.StatusTooManyRequests,
							fmt.Sprintf("您已达到该渠道的请求速率限制：每 %d 分钟内最多请求 %d 次", durationMinutes, maxCount))
						return
					}
				}
			}
		}

		// ── 模型级别速率限制 ──────────────────────────────────────────
		if len(otherSettings.ModelRateLimits) > 0 && channelId > 0 && userId > 0 {
			// 获取当前请求的模型名（由 Distribute 中间件设置）
			modelName := c.GetString("original_model")
			if modelName != "" {
				if modelCfg, found := otherSettings.ModelRateLimits[modelName]; found && modelCfg.Count > 0 {
					if modelCfg.LimitAdmins || userRole < common.RoleAdminUser {
						durationMinutes := modelCfg.DurationMinutes
						if durationMinutes <= 0 {
							// 继承渠道配置
							durationMinutes = otherSettings.ChannelRateLimitDuration
						}
						if durationMinutes <= 0 {
							durationMinutes = 1
						}
						duration := int64(durationMinutes * 60)

						key := fmt.Sprintf("%s:model:%d:%d:%s", ChannelRateLimitMark, channelId, userId, modelName)

						var allowed bool
						if common.RedisEnabled {
							allowed = channelRedisRateLimit(key, modelCfg.Count, duration, durationMinutes)
						} else {
							inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
							allowed = inMemoryRateLimiter.Request(key, modelCfg.Count, duration)
						}
						if !allowed {
							abortWithOpenAiMessage(c, http.StatusTooManyRequests,
								fmt.Sprintf("您已达到模型 %s 的请求速率限制：每 %d 分钟内最多请求 %d 次", modelName, durationMinutes, modelCfg.Count))
							return
						}
					}
				}
			}
		}

		c.Next()
	}
}

// channelRedisRateLimit 使用 Redis 实现滑动窗口限流，返回是否允许此次请求。
func channelRedisRateLimit(key string, maxCount int, duration int64, durationMinutes int) bool {
	ctx := context.Background()
	rdb := common.RDB
	expiry := time.Duration(durationMinutes) * time.Minute

	listLength, err := rdb.LLen(ctx, key).Result()
	if err != nil {
		// Redis 故障时放行，避免影响正常业务
		return true
	}

	if listLength < int64(maxCount) {
		rdb.LPush(ctx, key, time.Now().Format(timeFormat))
		rdb.Expire(ctx, key, expiry)
		return true
	}

	// 检查最早记录是否超出时间窗口
	oldTimeStr, _ := rdb.LIndex(ctx, key, -1).Result()
	oldTime, err := time.Parse(timeFormat, oldTimeStr)
	if err != nil {
		return true
	}

	nowTime, err := time.Parse(timeFormat, time.Now().Format(timeFormat))
	if err != nil {
		return true
	}

	// 时间窗口内已达到上限
	if int64(nowTime.Sub(oldTime).Seconds()) < duration {
		rdb.Expire(ctx, key, expiry)
		return false
	}

	// 时间窗口已过，滑入新记录
	rdb.LPush(ctx, key, time.Now().Format(timeFormat))
	rdb.LTrim(ctx, key, 0, int64(maxCount-1))
	rdb.Expire(ctx, key, expiry)
	return true
}
