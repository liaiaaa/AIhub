package main

// 极简 SQLite 只读解析器：只用来读 MiMo 桌面端 Cookies 库里的几个 cookie。
//
// 为什么不用第三方驱动：这个项目一直是零依赖、离线可编译的（build.bat 直接 go build）。
// 引入 modernc.org/sqlite 会让打包依赖 GOPROXY 网络。这里只需要「扫一张小表的若干行」，
// 自己解析 B-tree 足够，而且完全可控。
//
// 只实现：
//   - 表 B-tree（页类型 0x05 内部 / 0x0D 叶子）
//   - 记录（record）解码：varint 头 + serial types
//   - 溢出页跳过（cookie 行很小，用不到，但遇到也不会错）
// 不实现：索引、WAL 合并、写入、UTF-16（Chromium 用 UTF-8）。

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

type sqliteDB struct {
	data     []byte
	pageSize int
}

func openSQLiteFile(path string) (*sqliteDB, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 100 || string(b[:15]) != "SQLite format 3" {
		return nil, fmt.Errorf("不是 SQLite 文件：%s", path)
	}
	ps := int(binary.BigEndian.Uint16(b[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps > 65536 || (ps&(ps-1)) != 0 {
		return nil, fmt.Errorf("异常的 page size: %d", ps)
	}
	enc := binary.BigEndian.Uint32(b[56:60])
	if enc == 2 || enc == 3 {
		return nil, fmt.Errorf("暂不支持 UTF-16 编码的 SQLite")
	}
	return &sqliteDB{data: b, pageSize: ps}, nil
}

func (d *sqliteDB) page(n int) ([]byte, error) {
	off := (n - 1) * d.pageSize
	if n < 1 || off+d.pageSize > len(d.data) {
		return nil, fmt.Errorf("页号越界: %d", n)
	}
	return d.data[off : off+d.pageSize], nil
}

// readVarint 读取 SQLite 的变长整数（大端、7 位一组）。
func readVarint(b []byte, off int) (uint64, int) {
	var v uint64
	for i := 0; i < 9; i++ {
		if off+i >= len(b) {
			return v, i
		}
		c := b[off+i]
		if i == 8 {
			return (v << 8) | uint64(c), 9
		}
		v = (v << 7) | uint64(c&0x7f)
		if c < 0x80 {
			return v, i + 1
		}
	}
	return v, 9
}

// cellValues 解码一条记录（record），返回各列的原始值。
// 只关心文本/整数，其余返回 nil。
func decodeRecord(payload []byte) []any {
	hdrLen, n := readVarint(payload, 0)
	if hdrLen == 0 || int(hdrLen) > len(payload) {
		return nil
	}
	types := []uint64{}
	p := n
	for p < int(hdrLen) {
		t, adv := readVarint(payload, p)
		if adv == 0 {
			break
		}
		types = append(types, t)
		p += adv
	}
	body := int(hdrLen)
	vals := make([]any, 0, len(types))
	for _, t := range types {
		switch {
		case t == 0:
			vals = append(vals, nil)
		case t >= 1 && t <= 6:
			size := []int{0, 1, 2, 3, 4, 6, 8}[t]
			if body+size > len(payload) {
				return vals
			}
			var iv int64
			for i := 0; i < size; i++ {
				iv = (iv << 8) | int64(payload[body+i])
			}
			// 负数补码
			if size < 8 {
				shift := uint(64 - 8*size)
				iv = (iv << shift) >> shift
			}
			vals = append(vals, iv)
			body += size
		case t == 7:
			body += 8
			vals = append(vals, nil)
		case t == 8:
			vals = append(vals, int64(0))
		case t == 9:
			vals = append(vals, int64(1))
		case t >= 12 && t%2 == 0:
			size := int(t-12) / 2
			if body+size > len(payload) {
				return vals
			}
			vals = append(vals, nil) // blob 不关心
			body += size
		case t >= 13:
			size := int(t-13) / 2
			if body+size > len(payload) {
				return vals
			}
			vals = append(vals, string(payload[body:body+size]))
			body += size
		default:
			return vals
		}
	}
	return vals
}

// scanTable 遍历一张表的 B-tree，对每行调用 fn。fn 返回 true 表示提前结束。
func (d *sqliteDB) scanTable(root int, fn func(vals []any) bool) error {
	visited := map[int]bool{}
	var walk func(pageNo int) (bool, error)
	walk = func(pageNo int) (bool, error) {
		if pageNo < 1 || visited[pageNo] {
			return false, nil
		}
		visited[pageNo] = true
		pg, err := d.page(pageNo)
		if err != nil {
			return false, err
		}
		typ := pg[0]
		// 第 1 页开头有 100 字节的文件头，B-tree 页头从 100 开始；
		// 其余页的页头就在页首。这是最容易踩的一个坑。
		hdrOff := 0
		if pageNo == 1 {
			hdrOff = 100
			if hdrOff >= len(pg) {
				return false, nil
			}
			typ = pg[hdrOff]
		}
		if typ != 0x05 && typ != 0x0d {
			return false, nil // 不是表 B-tree，跳过
		}
		nCells := int(binary.BigEndian.Uint16(pg[hdrOff+3 : hdrOff+5]))
		hdrLen := 8
		if typ == 0x05 {
			hdrLen = 12
		}
		ptrBase := hdrOff + hdrLen
		for i := 0; i < nCells; i++ {
			pos := ptrBase + i*2
			if pos+2 > len(pg) {
				break
			}
			cellOff := int(binary.BigEndian.Uint16(pg[pos : pos+2]))
			if cellOff <= 0 || cellOff >= len(pg) {
				continue
			}
			if typ == 0x05 {
				// 内部页：4 字节左子页号 + varint key
				if cellOff+4 > len(pg) {
					continue
				}
				child := int(binary.BigEndian.Uint32(pg[cellOff : cellOff+4]))
				stop, err := walk(child)
				if err != nil {
					return false, err
				}
				if stop {
					return true, nil
				}
				continue
			}
			// 叶子页：varint payloadLen, varint rowid, payload
			plen, n1 := readVarint(pg, cellOff)
			_, n2 := readVarint(pg, cellOff+n1)
			start := cellOff + n1 + n2
			// 局部载荷：不处理溢出（cookie 行不会溢出）
			end := start + int(plen)
			if end > len(pg) {
				end = len(pg)
			}
			if start >= end {
				continue
			}
			vals := decodeRecord(pg[start:end])
			if len(vals) > 0 && fn(vals) {
				return true, nil
			}
		}
		if typ == 0x05 {
			// 最右子页（内部页头 +8 处的 4 字节）
			right := int(binary.BigEndian.Uint32(pg[hdrOff+8 : hdrOff+12]))
			return walk(right)
		}
		return false, nil
	}
	_, err := walk(root)
	return err
}

// sqliteFindTableRoot 在 sqlite_master 里找表的根页号。
func (d *sqliteDB) findTableRoot(table string) (int, error) {
	root := 0
	err := d.scanTable(1, func(vals []any) bool {
		if len(vals) < 4 {
			return false
		}
		typ, _ := vals[0].(string)
		name, _ := vals[1].(string)
		if typ == "table" && strings.EqualFold(name, table) {
			if rp, ok := vals[3].(int64); ok {
				root = int(rp)
				return true
			}
		}
		return false
	})
	if err != nil {
		return 0, err
	}
	if root == 0 {
		return 0, fmt.Errorf("找不到表 %s", table)
	}
	return root, nil
}

// readCookies 读取 Chromium Cookies 库，返回 name -> value。
// 只取指定名字，避免把整库拉进内存。
func readCookies(path string, want ...string) (map[string]string, error) {
	db, err := openSQLiteFile(path)
	if err != nil {
		return nil, err
	}
	root, err := db.findTableRoot("cookies")
	if err != nil {
		return nil, err
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	out := map[string]string{}
	_ = db.scanTable(root, func(vals []any) bool {
		if len(vals) < 5 {
			return false
		}
		name, _ := vals[3].(string)
		value, _ := vals[4].(string)
		if wantSet[name] && value != "" {
			out[name] = value
		}
		return false
	})
	return out, nil
}
