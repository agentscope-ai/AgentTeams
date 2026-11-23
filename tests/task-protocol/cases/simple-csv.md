# 简单任务：CSV 分类金额汇总

通过当前团队完成一个 Python 标准库小项目，不创建 Project 或额外成员。
Leader 使用一个执行子任务交付，保留原请求回告路由。

实现 `app.py`，支持：

```text
python app.py --input input.csv --output summary.json
```

输入为 UTF-8 CSV，表头严格为 `id,category,amount`。id 非空且唯一，category 非空。
金额为十进制字符串，格式 `-?(0|[1-9][0-9]*)\.[0-9]{2}`，允许负金额；不得用
浮点数累加。不自动修剪字段。非法表头、缺列、多列、重复 id、空字段、金额格式错误
均退出码 2，不写入输出文件。有效空数据（只有表头）成功。

输出 JSON 结构如下，amount 为精确两位小数字符串；零统一为 `0.00`：

```json
{"count":3,"amount":"10.25","categories":[{"category":"A","count":2,"amount":"5.25"},{"category":"B","count":1,"amount":"5.00"}]}
```

categories 按 category 的 Unicode 字符顺序排序；JSON 对象键顺序不限。
输入行重排不得改变结果。有效输入退出码 0，不调用网络，不依赖第三方包。

公开输入示例：

```csv
id,category,amount
a,A,10.00
b,B,5.00
c,A,-4.75
```

交付 app.py、可运行的 unittest 测试、简短 README 和实际测试结果。README 说明执行
和错误行为。通过任务协议提交真实文件，由 Leader 按原始要求验收，并在原请求会话
报告交付结果。不得仅以聊天声明或本地生成文件代替正式提交。

独立验收使用同一输入域的新数据，覆盖空数据、输入顺序、负金额与上述非法情况。
