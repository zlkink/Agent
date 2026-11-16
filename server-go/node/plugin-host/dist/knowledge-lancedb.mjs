// ../../../packages/plugin-sdk/src/index.ts
function definePlugin(plugin) {
  return plugin;
}

// ../../../plugins/knowledge-lancedb/src/index.ts
var TABLE = "chunks";
function sqlString(value) {
  return `'${value.replaceAll("'", "''")}'`;
}
var LanceStore = class _LanceStore {
  table;
  db;
  constructor(db, table) {
    this.db = db;
    this.table = table;
  }
  static async open(options) {
    const { connect } = await import("@lancedb/lancedb").catch((err) => {
      if (err?.code === "ERR_MODULE_NOT_FOUND") throw new Error("\u5411\u91CF\u5E93\u7EC4\u4EF6\uFF08LanceDB\uFF09\u5C1A\u672A\u5B89\u88C5\uFF1A\u8BF7\u5728\u77E5\u8BC6\u5E93\u9875\u9762\u70B9\u201C\u5B89\u88C5\u201D");
      throw new Error(`\u5F53\u524D\u5E73\u53F0\u4E0D\u652F\u6301 LanceDB \u5411\u91CF\u5E93\uFF1A${err instanceof Error ? err.message.split("\n")[0] : String(err)}`);
    });
    const db = await connect(options.dataDir);
    const names = await db.tableNames();
    return new _LanceStore(db, names.includes(TABLE) ? await db.openTable(TABLE) : void 0);
  }
  async upsert(chunks) {
    if (!chunks.length) return;
    const rows = chunks.map((c) => ({ id: c.id, document_id: c.documentId, title: c.title, text: c.text, vector: c.vector }));
    if (!this.table) {
      this.table = await this.db.createTable(TABLE, rows);
      return;
    }
    await this.table.mergeInsert("id").whenMatchedUpdateAll().whenNotMatchedInsertAll().execute(rows);
  }
  async search(vector, topK) {
    if (!this.table) return [];
    const rows = await this.table.vectorSearch(vector).distanceType("cosine").limit(topK).toArray();
    return rows.map((r) => ({ documentId: r.document_id, title: r.title, text: r.text, score: 1 - r._distance }));
  }
  async deleteDocument(documentId) {
    await this.table?.delete(`document_id = ${sqlString(documentId)}`);
  }
  async count() {
    return this.table ? this.table.countRows() : 0;
  }
  async drop() {
    if (!this.table) return;
    this.table.close();
    this.table = void 0;
    await this.db.dropTable(TABLE);
  }
};
var index_default = definePlugin({
  name: "knowledge-lancedb",
  version: "0.1.0",
  setup(ctx) {
    ctx.knowledgeStore("lancedb", (options) => LanceStore.open(options));
  }
});
export {
  LanceStore,
  index_default as default
};
