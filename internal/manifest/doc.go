// Package manifest 负责资源清单：拉取、解析、校验、缓存与降级。
//
// 负责：
//   - 三类清单（client.json / components/index.json / components/<key>.json）的加载；
//   - 多镜像前缀拼接与测速择优（清单本身的拉取）；
//   - ETag 条件请求与本地缓存；
//   - 网络失败时的降级顺序：在线 → 缓存 → 出厂快照，并且必须给出"当前用的是哪一份、多旧"；
//   - schema 版本兼容（未知字段忽略、未知 schema 降级而非崩溃）。
//
// 不负责：
//   - 决定"装到哪"（那是 install / paths 的事）；
//   - 下载安装包（那是 download 的事）。
//
// 关键约束：出厂快照过期时绝不静默使用，必须让用户看见。详见设计文档 04。
package manifest
