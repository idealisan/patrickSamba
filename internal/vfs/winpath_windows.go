package vfs

// hostNormalizesTrailingDotSpace 表示宿主的路径层会裁掉每个分量结尾的
// 点和空格，从而让一个对象拥有多个名字。
//
// Windows：true。Win32 的路径规范化就是这么做的，见 winpath.go 文件头。
//
// 这是编译期常量，所以 `if hostNormalizesTrailingDotSpace` 在另一侧平台
// 上会被死代码消除，零运行时开销。
const hostNormalizesTrailingDotSpace = true
