package store

import "crypto/rand"

// randRead 包装 crypto/rand.Read，便于测试替换。
var randRead = rand.Read
