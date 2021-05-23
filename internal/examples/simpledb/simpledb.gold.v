(* autogenerated from simpledb *)
From Perennial.goose_lang Require Import prelude.
Section code.
Context `{ext_ty: ext_types}.
Local Coercion Var' s: expr := Var s.

(* Package simpledb implements a one-table version of LevelDB

   It buffers all writes in memory; to make data durable, call Compact().
   This operation re-writes all of the data in the database
   (including in-memory writes) in a crash-safe manner.
   Keys in the table are cached for efficient reads. *)

(* A Table provides access to an immutable copy of data on the filesystem,
   along with an index for fast random access. *)
Definition Table := struct.decl [
  "Index" :: mapT uint64T;
  "File" :: fileT
].

(* CreateTable creates a new, empty table. *)
Definition CreateTable: val :=
  rec: "CreateTable" "p" :=
    let: "index" := NewMap uint64T in
    let: ("f", <>) := FS.create #(str"db") "p" in
    FS.close "f";;
    let: "f2" := FS.open #(str"db") "p" in
    struct.mk Table [
      "Index" ::= "index";
      "File" ::= "f2"
    ].

(* Entry represents a (key, value) pair. *)
Definition Entry := struct.decl [
  "Key" :: uint64T;
  "Value" :: slice.T byteT
].

(* DecodeUInt64 is a Decoder(uint64)

   All decoders have the shape func(p []byte) (T, uint64)

   The uint64 represents the number of bytes consumed; if 0,
   then decoding failed, and the value of type T should be ignored. *)
Definition DecodeUInt64: val :=
  rec: "DecodeUInt64" "p" :=
    (if: slice.len "p" < #8
    then (#0, #0)
    else
      let: "n" := UInt64Get "p" in
      ("n", #8)).

(* DecodeEntry is a Decoder(Entry) *)
Definition DecodeEntry: val :=
  rec: "DecodeEntry" "data" :=
    let: ("key", "l1") := DecodeUInt64 "data" in
    (if: ("l1" = #0)
    then
      (struct.mk Entry [
         "Key" ::= #0;
         "Value" ::= slice.nil
       ], #0)
    else
      let: ("valueLen", "l2") := DecodeUInt64 (SliceSkip byteT "data" "l1") in
      (if: ("l2" = #0)
      then
        (struct.mk Entry [
           "Key" ::= #0;
           "Value" ::= slice.nil
         ], #0)
      else
        (if: slice.len "data" < "l1" + "l2" + "valueLen"
        then
          (struct.mk Entry [
             "Key" ::= #0;
             "Value" ::= slice.nil
           ], #0)
        else
          let: "value" := SliceSubslice byteT "data" ("l1" + "l2") ("l1" + "l2" + "valueLen") in
          (struct.mk Entry [
             "Key" ::= "key";
             "Value" ::= "value"
           ], "l1" + "l2" + "valueLen")))).

Definition lazyFileBuf := struct.decl [
  "offset" :: uint64T;
  "next" :: slice.T byteT
].

(* readTableIndex parses a complete table on disk into a key->offset index *)
Definition readTableIndex: val :=
  rec: "readTableIndex" "f" "index" :=
    let: "buf" := ref_to (struct.t lazyFileBuf) (struct.mk lazyFileBuf [
      "offset" ::= #0;
      "next" ::= slice.nil
    ]) in
    (for: (λ: <>, #true); (λ: <>, Skip) := λ: <>,
      let: ("e", "l") := DecodeEntry (struct.get lazyFileBuf "next" (![struct.t lazyFileBuf] "buf")) in
      (if: "l" > #0
      then
        MapInsert "index" (struct.get Entry "Key" "e") (#8 + struct.get lazyFileBuf "offset" (![struct.t lazyFileBuf] "buf"));;
        "buf" <-[struct.t lazyFileBuf] struct.mk lazyFileBuf [
          "offset" ::= struct.get lazyFileBuf "offset" (![struct.t lazyFileBuf] "buf") + "l";
          "next" ::= SliceSkip byteT (struct.get lazyFileBuf "next" (![struct.t lazyFileBuf] "buf")) "l"
        ];;
        Continue
      else
        let: "p" := FS.readAt "f" (struct.get lazyFileBuf "offset" (![struct.t lazyFileBuf] "buf") + slice.len (struct.get lazyFileBuf "next" (![struct.t lazyFileBuf] "buf"))) #4096 in
        (if: (slice.len "p" = #0)
        then Break
        else
          let: "newBuf" := SliceAppendSlice byteT (struct.get lazyFileBuf "next" (![struct.t lazyFileBuf] "buf")) "p" in
          "buf" <-[struct.t lazyFileBuf] struct.mk lazyFileBuf [
            "offset" ::= struct.get lazyFileBuf "offset" (![struct.t lazyFileBuf] "buf");
            "next" ::= "newBuf"
          ];;
          Continue));;
      Continue).

(* RecoverTable restores a table from disk on startup. *)
Definition RecoverTable: val :=
  rec: "RecoverTable" "p" :=
    let: "index" := NewMap uint64T in
    let: "f" := FS.open #(str"db") "p" in
    readTableIndex "f" "index";;
    struct.mk Table [
      "Index" ::= "index";
      "File" ::= "f"
    ].

(* CloseTable frees up the fd held by a table. *)
Definition CloseTable: val :=
  rec: "CloseTable" "t" :=
    FS.close (struct.get Table "File" "t").

Definition readValue: val :=
  rec: "readValue" "f" "off" :=
    let: "startBuf" := FS.readAt "f" "off" #512 in
    let: "totalBytes" := UInt64Get "startBuf" in
    let: "buf" := SliceSkip byteT "startBuf" #8 in
    let: "haveBytes" := slice.len "buf" in
    (if: "haveBytes" < "totalBytes"
    then
      let: "buf2" := FS.readAt "f" ("off" + #512) ("totalBytes" - "haveBytes") in
      let: "newBuf" := SliceAppendSlice byteT "buf" "buf2" in
      "newBuf"
    else SliceTake "buf" "totalBytes").

Definition tableRead: val :=
  rec: "tableRead" "t" "k" :=
    let: ("off", "ok") := MapGet (struct.get Table "Index" "t") "k" in
    (if: ~ "ok"
    then (slice.nil, #false)
    else
      let: "p" := readValue (struct.get Table "File" "t") "off" in
      ("p", #true)).

Definition bufFile := struct.decl [
  "file" :: fileT;
  "buf" :: refT (slice.T byteT)
].

Definition newBuf: val :=
  rec: "newBuf" "f" :=
    let: "buf" := ref (zero_val (slice.T byteT)) in
    struct.mk bufFile [
      "file" ::= "f";
      "buf" ::= "buf"
    ].

Definition bufFlush: val :=
  rec: "bufFlush" "f" :=
    let: "buf" := ![slice.T byteT] (struct.get bufFile "buf" "f") in
    (if: (slice.len "buf" = #0)
    then #()
    else
      FS.append (struct.get bufFile "file" "f") "buf";;
      struct.get bufFile "buf" "f" <-[slice.T byteT] slice.nil).

Definition bufAppend: val :=
  rec: "bufAppend" "f" "p" :=
    let: "buf" := ![slice.T byteT] (struct.get bufFile "buf" "f") in
    let: "buf2" := SliceAppendSlice byteT "buf" "p" in
    struct.get bufFile "buf" "f" <-[slice.T byteT] "buf2".

Definition bufClose: val :=
  rec: "bufClose" "f" :=
    bufFlush "f";;
    FS.close (struct.get bufFile "file" "f").

Definition tableWriter := struct.decl [
  "index" :: mapT uint64T;
  "name" :: stringT;
  "file" :: struct.t bufFile;
  "offset" :: refT uint64T
].

Definition newTableWriter: val :=
  rec: "newTableWriter" "p" :=
    let: "index" := NewMap uint64T in
    let: ("f", <>) := FS.create #(str"db") "p" in
    let: "buf" := newBuf "f" in
    let: "off" := ref (zero_val uint64T) in
    struct.mk tableWriter [
      "index" ::= "index";
      "name" ::= "p";
      "file" ::= "buf";
      "offset" ::= "off"
    ].

Definition tableWriterAppend: val :=
  rec: "tableWriterAppend" "w" "p" :=
    bufAppend (struct.get tableWriter "file" "w") "p";;
    let: "off" := ![uint64T] (struct.get tableWriter "offset" "w") in
    struct.get tableWriter "offset" "w" <-[uint64T] "off" + slice.len "p".

Definition tableWriterClose: val :=
  rec: "tableWriterClose" "w" :=
    bufClose (struct.get tableWriter "file" "w");;
    let: "f" := FS.open #(str"db") (struct.get tableWriter "name" "w") in
    struct.mk Table [
      "Index" ::= struct.get tableWriter "index" "w";
      "File" ::= "f"
    ].

(* EncodeUInt64 is an Encoder(uint64) *)
Definition EncodeUInt64: val :=
  rec: "EncodeUInt64" "x" "p" :=
    let: "tmp" := NewSlice byteT #8 in
    UInt64Put "tmp" "x";;
    let: "p2" := SliceAppendSlice byteT "p" "tmp" in
    "p2".

(* EncodeSlice is an Encoder([]byte) *)
Definition EncodeSlice: val :=
  rec: "EncodeSlice" "data" "p" :=
    let: "p2" := EncodeUInt64 (slice.len "data") "p" in
    let: "p3" := SliceAppendSlice byteT "p2" "data" in
    "p3".

Definition tablePut: val :=
  rec: "tablePut" "w" "k" "v" :=
    let: "tmp" := NewSlice byteT #0 in
    let: "tmp2" := EncodeUInt64 "k" "tmp" in
    let: "tmp3" := EncodeSlice "v" "tmp2" in
    let: "off" := ![uint64T] (struct.get tableWriter "offset" "w") in
    MapInsert (struct.get tableWriter "index" "w") "k" ("off" + slice.len "tmp2");;
    tableWriterAppend "w" "tmp3".

(* Database is a handle to an open database. *)
Definition Database := struct.decl [
  "wbuffer" :: refT (mapT (slice.T byteT));
  "rbuffer" :: refT (mapT (slice.T byteT));
  "bufferL" :: lockRefT;
  "table" :: struct.ptrT Table;
  "tableName" :: refT stringT;
  "tableL" :: lockRefT;
  "compactionL" :: lockRefT
].

Definition makeValueBuffer: val :=
  rec: "makeValueBuffer" <> :=
    let: "buf" := NewMap (slice.T byteT) in
    let: "bufPtr" := ref (zero_val (mapT (slice.T byteT))) in
    "bufPtr" <-[mapT (slice.T byteT)] "buf";;
    "bufPtr".

(* NewDb initializes a new database on top of an empty filesys. *)
Definition NewDb: val :=
  rec: "NewDb" <> :=
    let: "wbuf" := makeValueBuffer #() in
    let: "rbuf" := makeValueBuffer #() in
    let: "bufferL" := lock.new #() in
    let: "tableName" := #(str"table.0") in
    let: "tableNameRef" := ref (zero_val stringT) in
    "tableNameRef" <-[stringT] "tableName";;
    let: "table" := CreateTable "tableName" in
    let: "tableRef" := struct.alloc Table (zero_val (struct.t Table)) in
    struct.store Table "tableRef" "table";;
    let: "tableL" := lock.new #() in
    let: "compactionL" := lock.new #() in
    struct.mk Database [
      "wbuffer" ::= "wbuf";
      "rbuffer" ::= "rbuf";
      "bufferL" ::= "bufferL";
      "table" ::= "tableRef";
      "tableName" ::= "tableNameRef";
      "tableL" ::= "tableL";
      "compactionL" ::= "compactionL"
    ].

(* Read gets a key from the database.

   Returns a boolean indicating if the k was found and a non-nil slice with
   the value if k was in the database.

   Reflects any completed in-memory writes. *)
Definition Read: val :=
  rec: "Read" "db" "k" :=
    lock.acquire (struct.get Database "bufferL" "db");;
    let: "buf" := ![mapT (slice.T byteT)] (struct.get Database "wbuffer" "db") in
    let: ("v", "ok") := MapGet "buf" "k" in
    (if: "ok"
    then
      lock.release (struct.get Database "bufferL" "db");;
      ("v", #true)
    else
      let: "rbuf" := ![mapT (slice.T byteT)] (struct.get Database "rbuffer" "db") in
      let: ("v2", "ok") := MapGet "rbuf" "k" in
      (if: "ok"
      then
        lock.release (struct.get Database "bufferL" "db");;
        ("v2", #true)
      else
        lock.acquire (struct.get Database "tableL" "db");;
        let: "tbl" := struct.load Table (struct.get Database "table" "db") in
        let: ("v3", "ok") := tableRead "tbl" "k" in
        lock.release (struct.get Database "tableL" "db");;
        lock.release (struct.get Database "bufferL" "db");;
        ("v3", "ok"))).

(* Write sets a key to a new value.

   Creates a new key-value mapping if k is not in the database and overwrites
   the previous value if k is present.

   The new value is buffered in memory. To persist it, call db.Compact(). *)
Definition Write: val :=
  rec: "Write" "db" "k" "v" :=
    lock.acquire (struct.get Database "bufferL" "db");;
    let: "buf" := ![mapT (slice.T byteT)] (struct.get Database "wbuffer" "db") in
    MapInsert "buf" "k" "v";;
    lock.release (struct.get Database "bufferL" "db").

Definition freshTable: val :=
  rec: "freshTable" "p" :=
    (if: ("p" = #(str"table.0"))
    then #(str"table.1")
    else
      (if: ("p" = #(str"table.1"))
      then #(str"table.0")
      else "p")).

Definition tablePutBuffer: val :=
  rec: "tablePutBuffer" "w" "buf" :=
    MapIter "buf" (λ: "k" "v",
      tablePut "w" "k" "v").

(* add all of table t to the table w being created; skip any keys in the (read)
   buffer b since those writes overwrite old ones *)
Definition tablePutOldTable: val :=
  rec: "tablePutOldTable" "w" "t" "b" :=
    let: "buf" := ref_to (struct.t lazyFileBuf) (struct.mk lazyFileBuf [
      "offset" ::= #0;
      "next" ::= slice.nil
    ]) in
    (for: (λ: <>, #true); (λ: <>, Skip) := λ: <>,
      let: ("e", "l") := DecodeEntry (struct.get lazyFileBuf "next" (![struct.t lazyFileBuf] "buf")) in
      (if: "l" > #0
      then
        let: (<>, "ok") := MapGet "b" (struct.get Entry "Key" "e") in
        (if: ~ "ok"
        then
          tablePut "w" (struct.get Entry "Key" "e") (struct.get Entry "Value" "e");;
          #()
        else #());;
        "buf" <-[struct.t lazyFileBuf] struct.mk lazyFileBuf [
          "offset" ::= struct.get lazyFileBuf "offset" (![struct.t lazyFileBuf] "buf") + "l";
          "next" ::= SliceSkip byteT (struct.get lazyFileBuf "next" (![struct.t lazyFileBuf] "buf")) "l"
        ];;
        Continue
      else
        let: "p" := FS.readAt (struct.get Table "File" "t") (struct.get lazyFileBuf "offset" (![struct.t lazyFileBuf] "buf") + slice.len (struct.get lazyFileBuf "next" (![struct.t lazyFileBuf] "buf"))) #4096 in
        (if: (slice.len "p" = #0)
        then Break
        else
          let: "newBuf" := SliceAppendSlice byteT (struct.get lazyFileBuf "next" (![struct.t lazyFileBuf] "buf")) "p" in
          "buf" <-[struct.t lazyFileBuf] struct.mk lazyFileBuf [
            "offset" ::= struct.get lazyFileBuf "offset" (![struct.t lazyFileBuf] "buf");
            "next" ::= "newBuf"
          ];;
          Continue));;
      Continue).

(* Build a new shadow table that incorporates the current table and a
   (write) buffer wbuf.

   Assumes all the appropriate locks have been taken.

   Returns the old table and new table. *)
Definition constructNewTable: val :=
  rec: "constructNewTable" "db" "wbuf" :=
    let: "oldName" := ![stringT] (struct.get Database "tableName" "db") in
    let: "name" := freshTable "oldName" in
    let: "w" := newTableWriter "name" in
    let: "oldTable" := struct.load Table (struct.get Database "table" "db") in
    tablePutOldTable "w" "oldTable" "wbuf";;
    tablePutBuffer "w" "wbuf";;
    let: "newTable" := tableWriterClose "w" in
    ("oldTable", "newTable").

(* Compact persists in-memory writes to a new table.

   This simple database design must re-write all data to combine in-memory
   writes with existing writes. *)
Definition Compact: val :=
  rec: "Compact" "db" :=
    lock.acquire (struct.get Database "compactionL" "db");;
    lock.acquire (struct.get Database "bufferL" "db");;
    let: "buf" := ![mapT (slice.T byteT)] (struct.get Database "wbuffer" "db") in
    let: "emptyWbuffer" := NewMap (slice.T byteT) in
    struct.get Database "wbuffer" "db" <-[mapT (slice.T byteT)] "emptyWbuffer";;
    struct.get Database "rbuffer" "db" <-[mapT (slice.T byteT)] "buf";;
    lock.release (struct.get Database "bufferL" "db");;
    lock.acquire (struct.get Database "tableL" "db");;
    let: "oldTableName" := ![stringT] (struct.get Database "tableName" "db") in
    let: ("oldTable", "t") := constructNewTable "db" "buf" in
    let: "newTable" := freshTable "oldTableName" in
    struct.store Table (struct.get Database "table" "db") "t";;
    struct.get Database "tableName" "db" <-[stringT] "newTable";;
    let: "manifestData" := Data.stringToBytes "newTable" in
    FS.atomicCreate #(str"db") #(str"manifest") "manifestData";;
    CloseTable "oldTable";;
    FS.delete #(str"db") "oldTableName";;
    lock.release (struct.get Database "tableL" "db");;
    lock.release (struct.get Database "compactionL" "db").

Definition recoverManifest: val :=
  rec: "recoverManifest" <> :=
    let: "f" := FS.open #(str"db") #(str"manifest") in
    let: "manifestData" := FS.readAt "f" #0 #4096 in
    let: "tableName" := Data.bytesToString "manifestData" in
    FS.close "f";;
    "tableName".

(* delete 'name' if it isn't tableName or "manifest" *)
Definition deleteOtherFile: val :=
  rec: "deleteOtherFile" "name" "tableName" :=
    (if: ("name" = "tableName")
    then #()
    else
      (if: ("name" = #(str"manifest"))
      then #()
      else FS.delete #(str"db") "name")).

Definition deleteOtherFiles: val :=
  rec: "deleteOtherFiles" "tableName" :=
    let: "files" := FS.list #(str"db") in
    let: "nfiles" := slice.len "files" in
    let: "i" := ref_to uint64T #0 in
    (for: (λ: <>, #true); (λ: <>, Skip) := λ: <>,
      (if: (![uint64T] "i" = "nfiles")
      then Break
      else
        let: "name" := SliceGet stringT "files" (![uint64T] "i") in
        deleteOtherFile "name" "tableName";;
        "i" <-[uint64T] ![uint64T] "i" + #1;;
        Continue)).

(* Recover restores a previously created database after a crash or shutdown. *)
Definition Recover: val :=
  rec: "Recover" <> :=
    let: "tableName" := recoverManifest #() in
    let: "table" := RecoverTable "tableName" in
    let: "tableRef" := struct.alloc Table (zero_val (struct.t Table)) in
    struct.store Table "tableRef" "table";;
    let: "tableNameRef" := ref (zero_val stringT) in
    "tableNameRef" <-[stringT] "tableName";;
    deleteOtherFiles "tableName";;
    let: "wbuffer" := makeValueBuffer #() in
    let: "rbuffer" := makeValueBuffer #() in
    let: "bufferL" := lock.new #() in
    let: "tableL" := lock.new #() in
    let: "compactionL" := lock.new #() in
    struct.mk Database [
      "wbuffer" ::= "wbuffer";
      "rbuffer" ::= "rbuffer";
      "bufferL" ::= "bufferL";
      "table" ::= "tableRef";
      "tableName" ::= "tableNameRef";
      "tableL" ::= "tableL";
      "compactionL" ::= "compactionL"
    ].

(* Shutdown immediately closes the database.

   Discards any uncommitted in-memory writes; similar to a crash except for
   cleanly closing any open files. *)
Definition Shutdown: val :=
  rec: "Shutdown" "db" :=
    lock.acquire (struct.get Database "bufferL" "db");;
    lock.acquire (struct.get Database "compactionL" "db");;
    let: "t" := struct.load Table (struct.get Database "table" "db") in
    CloseTable "t";;
    lock.release (struct.get Database "compactionL" "db");;
    lock.release (struct.get Database "bufferL" "db").

(* Close closes an open database cleanly, flushing any in-memory writes.

   db should not be used afterward *)
Definition Close: val :=
  rec: "Close" "db" :=
    Compact "db";;
    Shutdown "db".

End code.
