// Package app 承担应用组合与生命周期：service 注册/逆序关闭/supervisor、
// readiness barrier 与退出码（0/1/2/75）。
// 设计：docs/design/01-runtime-and-components.md §1。
package app
