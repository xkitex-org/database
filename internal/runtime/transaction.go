package runtime

import (
	"context"
	"errors"
	"sync"

	"xorm.io/xorm"
)

// transactionKey 是包私有的上下文键，业务不能伪造或覆盖事务绑定。
type transactionKey struct{}

// Transaction 独占一个原生 Session；锁覆盖整条 SQL 操作，避免并发污染 XORM 可变条件。
type Transaction struct {
	mu       sync.Mutex
	manager  *Manager
	source   string
	session  *xorm.Session
	ctx      context.Context
	ended    bool
	closeErr error
}

// boundTransaction 返回当前业务调用链的事务，取消或结束不会移除绑定。
func boundTransaction(ctx context.Context) *Transaction {
	if ctx == nil {
		return nil
	}
	tx, _ := ctx.Value(transactionKey{}).(*Transaction)
	return tx
}

// Begin 显式开启当前数据源事务并返回携带事务的新 ctx，不支持嵌套开启。
func (h *Handle) Begin(ctx context.Context) (context.Context, *Transaction, error) {
	if h == nil || h.manager == nil {
		return nil, nil, errors.New("database: 数据源句柄无效")
	}
	if ctx == nil {
		return nil, nil, errors.New("database: context 不能为空")
	}
	if boundTransaction(ctx) != nil {
		return nil, nil, errors.New("database: 不支持嵌套开启事务，请沿用已有 ctx")
	}
	m := h.manager
	m.mu.RLock()
	defer m.mu.RUnlock()
	name, engine, err := h.resolve()
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	session := engine.NewSession().Context(ctx)
	if err := session.Begin(); err != nil {
		_ = session.Close()
		return nil, nil, err
	}
	tx := &Transaction{manager: m, source: name, session: session, ctx: ctx}
	txCtx := context.WithValue(ctx, transactionKey{}, tx)
	m.txMu.Lock()
	m.transactions[tx] = struct{}{}
	m.txMu.Unlock()
	return txCtx, tx, nil
}

// Commit 提交并释放 Session，提交失败原样返回且该事务不可再次执行 SQL。
func (tx *Transaction) Commit() error {
	if tx == nil || tx.manager == nil {
		return errors.New("database: 事务句柄无效")
	}
	tx.mu.Lock()
	if tx.ended {
		tx.mu.Unlock()
		return errors.New("database: 事务已经结束")
	}
	tx.ended = true
	var err error
	if err = tx.ctx.Err(); err == nil {
		err = tx.session.Commit()
	} else {
		err = errors.Join(err, tx.session.Rollback())
	}
	tx.closeErr = tx.session.Close()
	err = errors.Join(err, tx.closeErr)
	tx.mu.Unlock()
	tx.forget()
	return err
}

// Rollback 回滚并释放 Session，已结束事务重复回滚等同于幂等清理。
func (tx *Transaction) Rollback() error { return tx.Close() }

// Close 回滚尚未提交的事务，适用于 defer；提交成功后不会再次回滚。
func (tx *Transaction) Close() error {
	if tx == nil || tx.manager == nil {
		return errors.New("database: 事务句柄无效")
	}
	tx.mu.Lock()
	if !tx.ended {
		tx.ended = true
		tx.closeErr = errors.Join(tx.session.Rollback(), tx.session.Close())
	}
	err := tx.closeErr
	tx.mu.Unlock()
	tx.forget()
	return err
}

// forget 在原生 Session 释放后移除应用资源跟踪，不改变 ctx 中的结束标志。
func (tx *Transaction) forget() {
	tx.manager.txMu.Lock()
	delete(tx.manager.transactions, tx)
	tx.manager.txMu.Unlock()
}
