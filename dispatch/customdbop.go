/*
创建时间: 2020/08/2020/8/25
作者: Administrator
功能介绍:
数据库操作接口
*/
package dispatch


//只返回单行数据
type CustomDBOperate interface {
	//必须设置数据库参数
	SetDBEventParam(param  *DBEventParam)
	//执行数据库查询
	ExcouteQueryFun() error
	//查询回调
	OnQueryCB() error
}

