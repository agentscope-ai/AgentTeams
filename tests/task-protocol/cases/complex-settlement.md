# 复杂任务：账单与退款结算

通过现有团队完成 Python 标准库项目，不创建 Project 或新成员。需要以下四个执行
节点：A 与 B 无依赖并行；C 依赖已验收的 A/B；D 独立验证 C。两名执行 Worker
按此依赖完成，Leader 负责计划和验收，不代做实现。

- A：实现 `transactions.py` 的 `load_transactions(path)`，读取账单 CSV，返回
  字典列表，字段 id、category、amount；amount 为整数分。交付模块、字段说明与测试。
- B：实现 `refunds.py` 的 `load_refunds(path)`，读取退款 CSV，返回字典列表，字段
  id、transaction_id、amount；amount 为整数分。交付模块、字段说明与测试。
- C：按任务输入引用获取 A/B 的已验收模块，在自己的输出目录集成 `app.py`，复用
  两个读取函数，不复制重写解析器。交付完整可执行项目、README、样例结果与测试。
- D：在自己的临时目录独立验证 C，交付可运行测试与实际验证报告；不修改上游输入。

主入口：

```text
python app.py --transactions transactions.csv --refunds refunds.csv --output summary.json
```

账单表头严格为 `id,category,amount`，退款表头严格为 `id,transaction_id,amount`。
两个文件均为 UTF-8 CSV；每个文件内 id 非空且唯一；category、transaction_id 非空。
金额为非负十进制，格式 `(0|[1-9][0-9]*)\.[0-9]{2}`，不得用浮点数累加。
不自动修剪字段。读取函数对非法表头、缺列、多列、空字段、重复 id、金额格式错误
抛出 ValueError；主入口捕获这些输入错误，退出码 2，不写入输出文件。

同一账单可有多次退款。匹配退款总额不能超过该账单金额，否则主入口报上述输入错误。
transaction_id 未匹配任何账单的退款不是错误，单独列出，不影响已匹配账单净额。
只有表头的空数据合法。有效输入退出码 0。

输出金额均为精确两位小数字符串，零为 `0.00`。示例结构：

```json
{
  "transactions":2,
  "gross":"30.00",
  "matchedRefunds":"3.00",
  "net":"27.00",
  "unmatchedRefunds":[{"id":"r3","transactionId":"missing","amount":"4.00"}],
  "categories":[
    {"category":"A","transactions":1,"gross":"10.00","refunds":"3.00","net":"7.00"},
    {"category":"B","transactions":1,"gross":"20.00","refunds":"0.00","net":"20.00"}
  ]
}
```

categories 按 category 排序，unmatchedRefunds 按退款 id 排序，均采用 Unicode 字符顺序。
JSON 对象键顺序不限；原始输入行重排不改变结果。

公开账单：

```csv
id,category,amount
t1,A,10.00
t2,B,20.00
```

公开退款：

```csv
id,transaction_id,amount
r1,t1,1.00
r2,t1,2.00
r3,missing,4.00
```

中间模块通过任务文件协议发布和引用。输入按确定产物版本使用，保持上游只读。
最终项目包含 app.py、transactions.py、refunds.py、README、测试及验证报告。
Leader 验收后完成主任务，并通过原请求会话交付真实产物。独立验收使用同一规则的
新数据，覆盖多次退款、未匹配退款、重排、空数据及上述非法输入。
