import * as React from "react";

import {colors} from "sourcegraph/components/jsStyles/colors";

const smallFont = 12.75;

const ResultRow = ({title, description, index, length}, categoryIndex, itemIndex, selected, delegate) => {
	let titleColor = colors.coolGray3();
	let backgroundColor = colors.coolGray1(.5);
	let descriptionColor = colors.coolGray3();
	if (selected) {
		titleColor = colors.coolGray1();
		backgroundColor = colors.coolGray3();
		descriptionColor = colors.coolGray1();
	}

	return (
		<a key={itemIndex} style={{
			borderRadius: 3,
			padding: 16,
			margin: "0 8px 8px 8px",
			backgroundColor: backgroundColor,
			display: "block",
		}}
		ref={(node) => { if (selected && node) { node.scrollIntoView(false); } }}
		onClick={() => delegate.select(categoryIndex, itemIndex)}>
		{length ? <div>
			<span style={{color: titleColor}}>{title.substr(0, index)}</span>
			<span style={{color: colors.white(), fontWeight: "bold"}}>{title.substr(index, length)}</span>
			<span style={{color: titleColor}}>{title.substr(index + length)}</span>
		</div> :
		<div style={{color: colors.white()}}>
			{title}
		</div>
		}
		<div style={{fontSize: smallFont, color: descriptionColor}}>
			{description}
		</div>
		</a>
	);
};

const ResultCategory = ({title, results, isLoading, selected = -1, delegate, categoryIndex}) => {
	if (isLoading) {
		return (
			<div style={{padding: "14px 0"}}>
				<span style={{color: colors.coolGray3()}}>{title} (loading...)</span>
			</div>
		);
	}
	if (results.length === 0) {
		return <div></div>;
	}
	return <div style={{padding: "14px 0"}}>
		<div style={{paddingBottom: "0.5em", color: colors.coolGray3()}}>{title}</div>
		{
			results.map((result, index) => {
				return ResultRow(result, categoryIndex, index, (index === selected), delegate);
			})
		}
	</div>;
};

export const ResultCategories = ({categories, limit, selection, delegate}) => {
	let sections: JSX.Element[] = [];
	categories.forEach((category, i) => {
		let results = category.Results;
		let selected = -1;
		if (i === selection[0]) {
			selected = selection[1];
		}
		sections.push(<ResultCategory key={category.Title} isLoading={category.IsLoading} categoryIndex={i} title={category.Title} selected={selected} results={results} delegate={delegate} />);
	});
	return <div style={{overflow: "auto"}}>
		{sections}
	</div>;
};
